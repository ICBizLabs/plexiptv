package hdhr

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"plexiptv/internal/store"
)

func TestEndpoints(t *testing.T) {
	dev := store.Device{ID: 1, Name: "plexiptv", DeviceID: "1A2B3C4D", Port: 34400, Tuners: 3}
	h := &Handlers{
		Device: func() (store.Device, bool) { return dev, true },
		Lineup: func(d store.Device, base string) []LineupItem {
			return []LineupItem{{GuideNumber: "1001", GuideName: "CNN", URL: base + "/stream/5"}}
		},
	}
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "http://192.168.1.10:34400/discover.json", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var d Discover
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.BaseURL != "http://192.168.1.10:34400" || d.LineupURL != "http://192.168.1.10:34400/lineup.json" ||
		d.TunerCount != 3 || d.DeviceID != "1A2B3C4D" {
		t.Errorf("discover = %+v", d)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "http://192.168.1.10:34400/lineup.json", nil))
	var items []LineupItem
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].URL != "http://192.168.1.10:34400/stream/5" || items[0].GuideNumber != "1001" {
		t.Errorf("lineup = %+v", items)
	}
}

func TestPacketRoundTrip(t *testing.T) {
	var p bytes.Buffer
	putTLV(&p, tagDeviceType, u32(deviceTypeTuner))
	putTLV(&p, tagBaseURL, bytes.Repeat([]byte("x"), 200)) // exercises 2-byte length
	pkt := encodePacket(typeDiscoverRpy, p.Bytes())
	typ, payload, err := decodePacket(pkt)
	if err != nil || typ != typeDiscoverRpy {
		t.Fatalf("decode: %v %x", err, typ)
	}
	var seen []byte
	walkTLV(payload, func(tag byte, v []byte) {
		seen = append(seen, tag)
		if tag == tagBaseURL && len(v) != 200 {
			t.Errorf("base url len %d", len(v))
		}
	})
	if len(seen) != 2 {
		t.Errorf("tags = %v", seen)
	}
	pkt[5] ^= 0xff
	if _, _, err := decodePacket(pkt); err == nil {
		t.Error("expected crc error")
	}
}
