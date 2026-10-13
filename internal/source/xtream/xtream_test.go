package xtream

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeLiveStreams(t *testing.T) {
	in := `[{"num":1,"name":"A","stream_id":12,"epg_channel_id":null,"category_id":"3"},
	        {"num":"2","name":"B","stream_id":"13","epg_channel_id":"b.us","category_id":4}]`
	var got []Stream
	if err := DecodeLiveStreams(strings.NewReader(in), func(s Stream) error { got = append(got, s); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].StreamID != "12" || got[1].StreamID != "13" || got[1].CategoryID != "4" || got[0].EPGChannelID != "" {
		t.Fatalf("got %+v", got)
	}
	if err := DecodeLiveStreams(strings.NewReader(`{"user_info":{"auth":0}}`), func(Stream) error { return nil }); err == nil {
		t.Fatal("expected error for non-list response")
	}
}

func TestStreamExt(t *testing.T) {
	cases := map[string]string{
		`{"allowed_output_formats":["m3u8","ts","rtmp"]}`: "ts",
		`{"allowed_output_formats":["m3u8"]}`:             "m3u8",
		`{"allowed_output_formats":[]}`:                   "ts",
		`{"allowed_output_formats":"weird"}`:              "ts",
		`{}`:                                              "ts",
	}
	for in, want := range cases {
		var u UserInfo
		if err := json.Unmarshal([]byte(in), &u); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got := u.StreamExt(); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
	c := New("provider.example:8080/player_api.php", "u s", "p/w", "", nil)
	if got := c.StreamURL("7", "m3u8"); got != "http://provider.example:8080/live/u%20s/p%2Fw/7.m3u8" {
		t.Errorf("stream url %s", got)
	}
}
