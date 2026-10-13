package ident

import (
	"net/http"
	"testing"
)

func TestResolveAndApply(t *testing.T) {
	defer SetDefault("")
	if Resolve("") != Presets[0].UA {
		t.Fatalf("default = %q", Resolve(""))
	}
	h := http.Header{}
	Apply(h, "", true)
	if h.Get("User-Agent") != Presets[0].UA || h.Get("Accept") != "*/*" || h.Get("Accept-Encoding") != "identity" {
		t.Fatalf("headers = %v", h)
	}
	SetDefault(Presets[3].UA)
	h = http.Header{}
	Apply(h, "", false)
	if h.Get("User-Agent") != Presets[3].UA || h.Get("Accept-Language") != "en_US" || h.Get("Accept-Encoding") != "" {
		t.Fatalf("headers = %v", h)
	}
	if Resolve("Bad\r\nX-Injected: 1") != "BadX-Injected: 1" {
		t.Fatalf("control characters kept: %q", Resolve("Bad\r\nX-Injected: 1"))
	}
	if Resolve(" Custom/1.0 ") != "Custom/1.0" {
		t.Fatal("source UA should win")
	}
	if a := FFmpegArgs("Custom/1.0"); len(a) != 4 || a[1] != "Custom/1.0" || a[3] != "Accept: */*\r\n" {
		t.Fatalf("ffmpeg args = %q", a)
	}
}
