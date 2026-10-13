// Package ident decides how plexiptv identifies itself to IPTV providers.
// Some providers reject or throttle unknown clients (Go's default
// "Go-http-client/1.1", ffmpeg's "Lavf/…"), so requests go out with the
// User-Agent and accompanying headers of a common TV or player app.
package ident

import (
	"net/http"
	"strings"
	"sync/atomic"
)

type Preset struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	UA      string            `json:"ua"`
	Headers map[string]string `json:"-"`
}

var tvHeaders = map[string]string{"Accept": "*/*", "Accept-Language": "en-US,en;q=0.9"}

// Presets are real-world client identities. The first one is the default.
var Presets = []Preset{
	{ID: "samsung", Name: "Samsung Smart TV (Tizen)", Headers: tvHeaders,
		UA: "Mozilla/5.0 (SMART-TV; LINUX; Tizen 6.5) AppleWebKit/537.36 (KHTML, like Gecko) 85.0.4183.93/6.5 TV Safari/537.36"},
	{ID: "lg", Name: "LG Smart TV (webOS)", Headers: tvHeaders,
		UA: "Mozilla/5.0 (Web0S; Linux/SmartTV) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/87.0.4280.88 Safari/537.36 WebAppManager"},
	{ID: "firetv", Name: "Amazon Fire TV Stick 4K (Android app)", Headers: map[string]string{},
		UA: "Dalvik/2.1.0 (Linux; U; Android 9; AFTMM Build/PS7633)"},
	{ID: "vlc", Name: "VLC media player", Headers: map[string]string{"Accept": "*/*", "Accept-Language": "en_US"},
		UA: "VLC/3.0.21 LibVLC/3.0.21"},
}

var defaultUA atomic.Value

// Default is the User-Agent used by sources that don't set their own.
func Default() string {
	if v, _ := defaultUA.Load().(string); v != "" {
		return v
	}
	return Presets[0].UA
}

// SetDefault changes the global User-Agent; blank restores the first preset.
func SetDefault(ua string) { defaultUA.Store(strings.TrimSpace(ua)) }

// Resolve returns a source's own User-Agent, or the global default.
func Resolve(ua string) string {
	ua = strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return -1
		}
		return r
	}, ua)
	if ua = strings.TrimSpace(ua); ua != "" {
		return ua
	}
	return Default()
}

func headersFor(ua string) map[string]string {
	for _, p := range Presets {
		if p.UA == ua {
			return p.Headers
		}
	}
	return map[string]string{"Accept": "*/*"}
}

// Apply sets the identity headers for ua on an outgoing request. Stream
// requests ask for an unencoded body, like media players do.
func Apply(h http.Header, ua string, stream bool) {
	ua = Resolve(ua)
	h.Set("User-Agent", ua)
	for k, v := range headersFor(ua) {
		h.Set(k, v)
	}
	if stream {
		h.Set("Accept-Encoding", "identity")
	}
}

// FFmpegArgs returns the ffmpeg input options that send the same identity.
func FFmpegArgs(ua string) []string {
	ua = Resolve(ua)
	args := []string{"-user_agent", ua}
	var b strings.Builder
	for k, v := range headersFor(ua) {
		b.WriteString(k + ": " + v + "\r\n")
	}
	if b.Len() > 0 {
		args = append(args, "-headers", b.String())
	}
	return args
}
