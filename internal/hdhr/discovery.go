package hdhr

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"plexiptv/internal/store"
)

// Announcer answers network discovery for all configured devices, using
// both SSDP (UDP 1900) and SiliconDust's own protocol (UDP 65001), which is
// what Plex uses to find tuners automatically. Both are best-effort: if a
// port is taken (e.g. Windows' SSDP Discovery service) the device can still
// be added in Plex by IP:port.
type Announcer struct {
	Devices   func() []store.Device
	Advertise string
	Log       *slog.Logger
}

func (a *Announcer) Run(ctx context.Context) {
	go a.runHDHR(ctx)
	go a.runSSDP(ctx)
}

// localIPFor returns the local address used to reach peer.
func (a *Announcer) localIPFor(peer *net.UDPAddr) string {
	if a.Advertise != "" {
		return a.Advertise
	}
	c, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: peer.IP, Port: peer.Port})
	if err != nil {
		return "127.0.0.1"
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.String()
}

func base(ip string, port int) string {
	return "http://" + net.JoinHostPort(ip, strconv.Itoa(port))
}

// ---- SiliconDust discovery (UDP 65001) ----

const (
	typeDiscoverReq = 0x0002
	typeDiscoverRpy = 0x0003
	tagDeviceType   = 0x01
	tagDeviceID     = 0x02
	tagTunerCount   = 0x10
	tagLineupURL    = 0x27
	tagBaseURL      = 0x2A
	tagDeviceAuth   = 0x2B
	deviceTypeTuner = 0x00000001
	wildcard        = 0xFFFFFFFF
)

func (a *Announcer) runHDHR(ctx context.Context) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 65001})
	if err != nil {
		a.Log.Warn("HDHomeRun discovery disabled (UDP 65001 unavailable); add the tuner in Plex by IP:port", "err", err)
		return
	}
	go func() { <-ctx.Done(); conn.Close() }()
	a.Log.Info("HDHomeRun discovery listening", "port", 65001)
	buf := make([]byte, 1500)
	for {
		n, peer, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		typ, payload, err := decodePacket(buf[:n])
		if err != nil || typ != typeDiscoverReq {
			continue
		}
		wantType, wantID := uint32(wildcard), uint32(wildcard)
		walkTLV(payload, func(tag byte, v []byte) {
			if len(v) == 4 {
				switch tag {
				case tagDeviceType:
					wantType = binary.BigEndian.Uint32(v)
				case tagDeviceID:
					wantID = binary.BigEndian.Uint32(v)
				}
			}
		})
		if wantType != wildcard && wantType != deviceTypeTuner {
			continue
		}
		ip := a.localIPFor(peer)
		for _, d := range a.Devices() {
			id, err := strconv.ParseUint(d.DeviceID, 16, 32)
			if err != nil || (wantID != wildcard && uint32(id) != wantID) {
				continue
			}
			b := base(ip, d.Port)
			var p bytes.Buffer
			putTLV(&p, tagDeviceType, u32(deviceTypeTuner))
			putTLV(&p, tagDeviceID, u32(uint32(id)))
			putTLV(&p, tagTunerCount, []byte{byte(min(max(d.Tuners, 1), 255))})
			putTLV(&p, tagBaseURL, []byte(b))
			putTLV(&p, tagLineupURL, []byte(b+"/lineup.json"))
			putTLV(&p, tagDeviceAuth, []byte("plexiptv"))
			conn.WriteToUDP(encodePacket(typeDiscoverRpy, p.Bytes()), peer)
		}
	}
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func putTLV(w *bytes.Buffer, tag byte, v []byte) {
	w.WriteByte(tag)
	if len(v) < 128 {
		w.WriteByte(byte(len(v)))
	} else {
		w.WriteByte(byte(len(v)&0x7f) | 0x80)
		w.WriteByte(byte(len(v) >> 7))
	}
	w.Write(v)
}

func walkTLV(p []byte, fn func(tag byte, v []byte)) {
	for len(p) >= 2 {
		tag := p[0]
		l := int(p[1])
		hdr := 2
		if l&0x80 != 0 {
			if len(p) < 3 {
				return
			}
			l = (l & 0x7f) | int(p[2])<<7
			hdr = 3
		}
		if len(p) < hdr+l {
			return
		}
		fn(tag, p[hdr:hdr+l])
		p = p[hdr+l:]
	}
}

func encodePacket(typ uint16, payload []byte) []byte {
	out := make([]byte, 4, 4+len(payload)+4)
	binary.BigEndian.PutUint16(out[0:], typ)
	binary.BigEndian.PutUint16(out[2:], uint16(len(payload)))
	out = append(out, payload...)
	return binary.LittleEndian.AppendUint32(out, crc32.ChecksumIEEE(out))
}

func decodePacket(b []byte) (uint16, []byte, error) {
	if len(b) < 8 {
		return 0, nil, errors.New("short packet")
	}
	l := int(binary.BigEndian.Uint16(b[2:]))
	if len(b) < 4+l+4 {
		return 0, nil, errors.New("truncated packet")
	}
	body := b[:4+l]
	if crc32.ChecksumIEEE(body) != binary.LittleEndian.Uint32(b[4+l:]) {
		return 0, nil, errors.New("bad crc")
	}
	return binary.BigEndian.Uint16(b), b[4 : 4+l], nil
}

// ---- SSDP (UDP 1900) ----

var ssdpAddr = &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}

func (a *Announcer) runSSDP(ctx context.Context) {
	conn, err := net.ListenMulticastUDP("udp4", nil, ssdpAddr)
	if err != nil {
		a.Log.Warn("SSDP disabled (UDP 1900 unavailable); add the tuner in Plex by IP:port", "err", err)
		return
	}
	go func() { <-ctx.Done(); conn.Close() }()
	a.Log.Info("SSDP listening", "port", 1900)
	buf := make([]byte, 2048)
	for {
		n, peer, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		msg := string(buf[:n])
		if !strings.HasPrefix(msg, "M-SEARCH") {
			continue
		}
		st := header(msg, "ST")
		switch st {
		case "ssdp:all", "upnp:rootdevice", "urn:schemas-upnp-org:device:MediaServer:1":
		default:
			continue
		}
		if st == "ssdp:all" {
			st = "upnp:rootdevice"
		}
		ip := a.localIPFor(peer)
		out, err := net.DialUDP("udp4", nil, peer)
		if err != nil {
			continue
		}
		for _, d := range a.Devices() {
			resp := fmt.Sprintf("HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=1800\r\nEXT:\r\nLOCATION: %s/device.xml\r\n"+
				"SERVER: plexiptv UPnP/1.0\r\nST: %s\r\nUSN: uuid:%s::%s\r\n\r\n",
				base(ip, d.Port), st, UUID(d.DeviceID), st)
			out.Write([]byte(resp))
		}
		out.Close()
	}
}

func header(msg, name string) string {
	for _, line := range strings.Split(msg, "\r\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(k), name) {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return ""
}
