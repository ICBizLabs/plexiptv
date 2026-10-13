package stream

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// tsServer streams endless fake TS packets and counts open connections.
func tsServer(open *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		open.Add(1)
		defer open.Add(-1)
		w.Header().Set("Content-Type", "video/mp2t")
		pkt := make([]byte, 188*100)
		for i := 0; i < len(pkt); i += 188 {
			pkt[i] = 0x47
		}
		for {
			if _, err := w.Write(pkt); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}))
}

func readSome(t *testing.T, ctx context.Context, url string) (*http.Response, error) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 200 {
		buf := make([]byte, 188*10)
		if _, err := io.ReadFull(resp.Body, buf); err != nil {
			t.Fatalf("read: %v", err)
		}
		if buf[0] != 0x47 {
			t.Fatalf("not TS data")
		}
	}
	return resp, nil
}

func TestSharedUpstreamAndLimit(t *testing.T) {
	var open atomic.Int32
	up := tsServer(&open)
	defer up.Close()

	m := NewManager("ffmpeg-does-not-exist", slog.New(slog.NewTextHandler(io.Discard, nil)))
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := int64(1)
		if r.URL.Path == "/2" {
			id = 2
		}
		m.Serve(w, r, Request{ChannelID: id, SourceID: 9, Name: r.URL.Path, URL: up.URL, Mode: "auto", Limit: 1})
	}))
	defer proxy.Close()

	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	r1, err := readSome(t, ctx1, proxy.URL+"/1")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := readSome(t, ctx2, proxy.URL+"/1")
	if err != nil {
		t.Fatal(err)
	}
	if n := open.Load(); n != 1 {
		t.Fatalf("two viewers of one channel should share one upstream, got %d", n)
	}

	// Limit is 1 and channel 1 holds it: channel 2 is refused.
	r3, err := readSome(t, context.Background(), proxy.URL+"/2")
	if err != nil {
		t.Fatal(err)
	}
	if r3.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", r3.StatusCode)
	}
	r3.Body.Close()

	// Both viewers leave: slot is released immediately so a channel change works.
	cancel1()
	cancel2()
	r1.Body.Close()
	r2.Body.Close()
	deadline := time.Now().Add(2 * time.Second)
	for len(m.Active()) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	ctx4, cancel4 := context.WithCancel(context.Background())
	defer cancel4()
	r4, err := readSome(t, ctx4, proxy.URL+"/2")
	if err != nil {
		t.Fatal(err)
	}
	if r4.StatusCode != 200 {
		t.Fatalf("channel change refused: %d", r4.StatusCode)
	}
	cancel4()
	r4.Body.Close()
	deadline = time.Now().Add(2 * time.Second)
	for open.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := open.Load(); n != 0 {
		t.Fatalf("upstream not closed after last viewer left: %d open", n)
	}
}
