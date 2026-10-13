// Package hdhr emulates the HTTP API of a SiliconDust HDHomeRun network tuner,
// which is what Plex Live TV & DVR talks to.
package hdhr

import (
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"strconv"

	"plexiptv/internal/store"
)

type LineupItem struct {
	GuideNumber string
	GuideName   string
	URL         string
	HD          int `json:",omitempty"`
}

type Discover struct {
	FriendlyName    string
	Manufacturer    string
	ModelNumber     string
	FirmwareName    string
	FirmwareVersion string
	DeviceID        string
	DeviceAuth      string
	BaseURL         string
	LineupURL       string
	TunerCount      int
}

func NewDiscover(d store.Device, base string) Discover {
	return Discover{
		FriendlyName:    d.Name,
		Manufacturer:    "Silicondust",
		ModelNumber:     "HDTC-2US",
		FirmwareName:    "hdhomeruntc_atsc",
		FirmwareVersion: "20200101",
		DeviceID:        d.DeviceID,
		DeviceAuth:      "plexiptv",
		BaseURL:         base,
		LineupURL:       base + "/lineup.json",
		TunerCount:      d.Tuners,
	}
}

// Handlers serves the HDHomeRun endpoints for whichever device currently
// owns the listening port.
type Handlers struct {
	Device    func() (store.Device, bool)
	Lineup    func(d store.Device, base string) []LineupItem
	Advertise string // optional host override
}

func (h *Handlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /discover.json", h.discover)
	mux.HandleFunc("GET /lineup_status.json", h.lineupStatus)
	mux.HandleFunc("GET /lineup.json", h.lineup)
	mux.HandleFunc("POST /lineup.post", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /lineup.post", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /device.xml", h.deviceXML)
	mux.HandleFunc("GET /capability", h.deviceXML)
}

// BaseURL is the address clients should use to reach this server: the Host
// they connected with, unless an advertise host is configured.
func BaseURL(r *http.Request, advertise string) string {
	host := r.Host
	if advertise != "" {
		_, port, err := net.SplitHostPort(r.Host)
		if err != nil {
			if la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
				_, port, _ = net.SplitHostPort(la.String())
			}
		}
		host = advertise
		if port != "" && port != "80" {
			host = net.JoinHostPort(advertise, port)
		}
	}
	return "http://" + host
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (h *Handlers) dev(w http.ResponseWriter) (store.Device, bool) {
	d, ok := h.Device()
	if !ok {
		http.Error(w, "no tuner device on this port", http.StatusNotFound)
	}
	return d, ok
}

func (h *Handlers) discover(w http.ResponseWriter, r *http.Request) {
	d, ok := h.dev(w)
	if !ok {
		return
	}
	writeJSON(w, NewDiscover(d, BaseURL(r, h.Advertise)))
}

func (h *Handlers) lineupStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ScanInProgress": 0, "ScanPossible": 1, "Source": "Cable", "SourceList": []string{"Cable"}})
}

func (h *Handlers) lineup(w http.ResponseWriter, r *http.Request) {
	d, ok := h.dev(w)
	if !ok {
		return
	}
	items := h.Lineup(d, BaseURL(r, h.Advertise))
	if items == nil {
		items = []LineupItem{}
	}
	writeJSON(w, items)
}

func (h *Handlers) deviceXML(w http.ResponseWriter, r *http.Request) {
	d, ok := h.dev(w)
	if !ok {
		return
	}
	base := BaseURL(r, h.Advertise)
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <specVersion><major>1</major><minor>0</minor></specVersion>
  <URLBase>%s</URLBase>
  <device>
    <deviceType>urn:schemas-upnp-org:device:MediaServer:1</deviceType>
    <friendlyName>%s</friendlyName>
    <manufacturer>Silicondust</manufacturer>
    <modelName>HDTC-2US</modelName>
    <modelNumber>HDTC-2US</modelNumber>
    <serialNumber>%s</serialNumber>
    <UDN>uuid:%s</UDN>
  </device>
</root>
`, html.EscapeString(base), html.EscapeString(d.Name), d.DeviceID, UUID(d.DeviceID))
}

// UUID derives a stable UPnP UUID from the 8-hex-digit device id.
func UUID(deviceID string) string {
	return "504c4558-4950-5456-0000-0000" + fmt.Sprintf("%08s", deviceID)
}

func GuideNumber(n int) string { return strconv.Itoa(n) }
