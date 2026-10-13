package m3u

import (
	"strings"
	"testing"
)

const sample = "\ufeff#EXTM3U url-tvg=\"http://epg.example/guide.xml.gz\"\n" +
	`#EXTINF:-1 tvg-id="cnn.us" tvg-name="CNN" tvg-logo="http://l/cnn.png" group-title="US | News",US: CNN, HD
http://prov.example:8080/live/user/pass/1234.ts
#EXTINF:-1 tvg-id="" group-title="Movies",Some Movie (2020)
http://prov.example:8080/movie/user/pass/555.mkv
#EXTINF:-1 group-title="Series",Show S01E01
http://prov.example:8080/series/user/pass/777.mp4
#EXTINF:-1,Plain Channel
#EXTGRP:Misc
#EXTVLCOPT:http-user-agent=foo
http://other.example/stream.m3u8
`

func TestParse(t *testing.T) {
	var hdr Header
	var got []Entry
	st, err := Parse(strings.NewReader(sample), func(h Header) { hdr = h }, func(e Entry) error {
		got = append(got, e)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if hdr.EPGURL != "http://epg.example/guide.xml.gz" {
		t.Errorf("epg url = %q", hdr.EPGURL)
	}
	if st.Live != 2 || st.Skipped != 2 {
		t.Fatalf("stats = %+v", st)
	}
	cnn := got[0]
	if cnn.Name != "US: CNN, HD" || cnn.TvgID != "cnn.us" || cnn.Group != "US | News" || cnn.Logo != "http://l/cnn.png" {
		t.Errorf("cnn = %+v", cnn)
	}
	if got[1].Name != "Plain Channel" || got[1].Group != "Misc" || got[1].URL != "http://other.example/stream.m3u8" {
		t.Errorf("plain = %+v", got[1])
	}
}

func TestStableKey(t *testing.T) {
	dup := map[string]int{}
	if k := StableKey(Entry{URL: "http://h:80/live/u/p/1234.ts"}, dup); k != "sid:1234" {
		t.Errorf("got %q", k)
	}
	if k := StableKey(Entry{URL: "http://h:80/u/p/42"}, dup); k != "sid:42" {
		t.Errorf("got %q", k)
	}
	a := StableKey(Entry{Name: "X", Group: "G", URL: "http://a/x.m3u8"}, dup)
	b := StableKey(Entry{Name: "X", Group: "G", URL: "http://b/x.m3u8"}, dup)
	if a == b {
		t.Errorf("duplicates should get distinct keys: %q %q", a, b)
	}
}
