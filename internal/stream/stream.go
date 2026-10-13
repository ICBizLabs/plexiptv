// Package stream relays provider streams to Plex. One upstream connection per
// channel is shared by every client watching it, provider connection limits
// are enforced, stalled upstreams are reconnected, and HLS sources can be
// remuxed to MPEG-TS through ffmpeg.
package stream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"plexiptv/internal/ident"
	"plexiptv/internal/redact"
)

const (
	chunkSize      = 188 * 348 // ~64 KiB, a whole number of TS packets
	minRead        = 188 * 64  // coalesce small upstream reads into ~12 KiB chunks
	clientMaxBytes = 32 << 20  // data queued for one client before it's dropped as too slow
	clientChunks   = 8192
	stallTimeout   = 20 * time.Second
	maxRetries     = 4
	errTunersBusy  = "All tuners are in use (provider connection limit reached)"
)

// Request describes what to play.
type Request struct {
	ChannelID int64
	SourceID  int64
	Name      string
	URL       string
	UserAgent string
	Mode      string // auto | proxy | ffmpeg | redirect
	Limit     int    // max concurrent upstreams for the source; 0 = unlimited
}

type Manager struct {
	FFmpeg string
	HTTP   *http.Client
	Log    *slog.Logger

	ffmpegOK bool
	mu       sync.Mutex
	sessions map[int64]*session
	active   map[int64]int // upstreams per source
}

func NewManager(ffmpeg string, log *slog.Logger) *Manager {
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	m := &Manager{
		FFmpeg:   ffmpeg,
		Log:      log,
		sessions: map[int64]*session{},
		active:   map[int64]int{},
		HTTP: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 15 * time.Second,
			IdleConnTimeout:       30 * time.Second,
			MaxIdleConnsPerHost:   4,
		}},
	}
	if p, err := exec.LookPath(ffmpeg); err == nil {
		m.FFmpeg, m.ffmpegOK = p, true
	} else {
		log.Warn("ffmpeg not found; HLS sources will be proxied as-is", "ffmpeg", ffmpeg)
	}
	return m
}

func (m *Manager) FFmpegAvailable() bool { return m.ffmpegOK }

type client struct {
	ch     chan []byte
	queued atomic.Int64 // bytes sitting in ch
	remote string
	since  time.Time
}

type session struct {
	m       *Manager
	req     Request
	ctx     context.Context
	cancel  context.CancelFunc
	started time.Time
	bytes   atomic.Int64
	mode    atomic.Value // string
	release sync.Once

	mu      sync.Mutex
	clients map[*client]struct{}
	closed  bool
}

// Serve plays a channel to w until the client disconnects.
func (m *Manager) Serve(w http.ResponseWriter, r *http.Request, req Request) {
	if req.Mode == "redirect" {
		http.Redirect(w, r, req.URL, http.StatusFound)
		return
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "video/mp2t")
		return
	}
	c, s, err := m.join(req, r.RemoteAddr)
	if err != nil {
		m.Log.Warn("stream refused", "channel", req.Name, "err", err)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer s.removeClient(c)
	m.Log.Info("client connected", "channel", req.Name, "client", r.RemoteAddr)

	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for {
		select {
		case <-r.Context().Done():
			return
		case buf, ok := <-c.ch:
			if !ok {
				return
			}
			c.queued.Add(-int64(len(buf)))
			if _, err := w.Write(buf); err != nil {
				return
			}
			if flusher != nil && len(c.ch) == 0 {
				flusher.Flush()
			}
		}
	}
}

func (m *Manager) join(req Request, remote string) (*client, *session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[req.ChannelID]
	if s == nil {
		if req.Limit > 0 && m.active[req.SourceID] >= req.Limit {
			return nil, nil, errors.New(errTunersBusy)
		}
		ctx, cancel := context.WithCancel(context.Background())
		s = &session{m: m, req: req, ctx: ctx, cancel: cancel, started: time.Now(), clients: map[*client]struct{}{}}
		s.mode.Store("")
		m.sessions[req.ChannelID] = s
		m.active[req.SourceID]++
		go s.run()
	}
	c := &client{ch: make(chan []byte, clientChunks), remote: remote, since: time.Now()}
	s.mu.Lock()
	s.clients[c] = struct{}{}
	s.mu.Unlock()
	return c, s, nil
}

// detach removes the session from the manager and frees its provider slot
// immediately, so a channel change in Plex isn't refused while the old
// upstream is still being torn down. Callers hold m.mu.
func (s *session) detachLocked() {
	if s.m.sessions[s.req.ChannelID] == s {
		delete(s.m.sessions, s.req.ChannelID)
	}
	s.release.Do(func() {
		s.m.active[s.req.SourceID]--
		if s.m.active[s.req.SourceID] <= 0 {
			delete(s.m.active, s.req.SourceID)
		}
	})
}

func (s *session) removeClient(c *client) {
	s.m.mu.Lock()
	s.mu.Lock()
	if _, ok := s.clients[c]; ok {
		delete(s.clients, c)
		close(c.ch)
	}
	last := len(s.clients) == 0 && !s.closed
	if last {
		s.closed = true
		s.detachLocked()
	}
	s.mu.Unlock()
	s.m.mu.Unlock()
	if last {
		s.cancel()
		s.m.Log.Info("stream stopped", "channel", s.req.Name, "duration", time.Since(s.started).Round(time.Second))
	}
}

// end is called when the upstream gives up; all clients are disconnected.
func (s *session) end() {
	s.m.mu.Lock()
	s.mu.Lock()
	s.closed = true
	s.detachLocked()
	for c := range s.clients {
		close(c.ch)
		delete(s.clients, c)
	}
	s.mu.Unlock()
	s.m.mu.Unlock()
	s.cancel()
}

func (s *session) broadcast(chunk []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.clients {
		if c.queued.Load()+int64(len(chunk)) <= clientMaxBytes {
			select {
			case c.ch <- chunk:
				c.queued.Add(int64(len(chunk)))
				continue
			default:
			}
		}
		s.m.Log.Warn("client too slow, disconnecting", "channel", s.req.Name, "client", c.remote)
		close(c.ch)
		delete(s.clients, c)
	}
	if len(s.clients) == 0 && !s.closed {
		go s.cancel()
	}
}

func (s *session) run() {
	defer s.end()
	failures := 0
	for s.ctx.Err() == nil {
		n, err := s.attempt()
		if s.ctx.Err() != nil {
			return
		}
		if n > 0 {
			failures = 0
		} else {
			failures++
		}
		msg := "upstream ended"
		if err != nil {
			msg = redact.URL(err.Error())
		}
		if failures >= maxRetries {
			s.m.Log.Error("giving up on upstream", "channel", s.req.Name, "err", msg)
			return
		}
		s.m.Log.Warn("upstream interrupted, reconnecting", "channel", s.req.Name, "err", msg, "attempt", failures+1)
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(time.Duration(failures+1) * 500 * time.Millisecond):
		}
	}
}

// attempt opens the upstream once and pumps it until it ends or stalls.
func (s *session) attempt() (int64, error) {
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	var stalled atomic.Bool
	watchdog := time.AfterFunc(stallTimeout, func() { stalled.Store(true); cancel() })
	defer watchdog.Stop()

	rc, mode, err := s.m.open(ctx, s.req)
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	s.mode.Store(mode)

	var total int64
	for {
		buf := make([]byte, chunkSize)
		n, err := io.ReadAtLeast(rc, buf, minRead)
		if n > 0 {
			watchdog.Reset(stallTimeout)
			total += int64(n)
			s.bytes.Add(int64(n))
			s.broadcast(buf[:n])
		}
		if err != nil {
			if stalled.Load() {
				return total, errors.New("no data for " + stallTimeout.String())
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return total, nil
			}
			return total, err
		}
	}
}

func isHLS(u string) bool {
	p := u
	if pu, err := url.Parse(u); err == nil {
		p = pu.Path
	}
	return strings.HasSuffix(strings.ToLower(p), ".m3u8")
}

func (m *Manager) open(ctx context.Context, req Request) (io.ReadCloser, string, error) {
	mode := req.Mode
	if mode == "" || mode == "auto" {
		mode = "proxy"
		if isHLS(req.URL) && m.ffmpegOK {
			mode = "ffmpeg"
		}
	}
	if mode == "ffmpeg" {
		if !m.ffmpegOK {
			return nil, "", errors.New("ffmpeg mode selected but ffmpeg was not found")
		}
		rc, err := m.openFFmpeg(ctx, req)
		return rc, "ffmpeg", err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL, nil)
	if err != nil {
		return nil, "", err
	}
	ident.Apply(hreq.Header, req.UserAgent, true)
	resp, err := m.HTTP.Do(hreq)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, "", fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
	}
	if ct := strings.ToLower(resp.Header.Get("Content-Type")); strings.Contains(ct, "mpegurl") {
		resp.Body.Close()
		if !m.ffmpegOK {
			return nil, "", errors.New("upstream is HLS and ffmpeg is not available")
		}
		rc, err := m.openFFmpeg(ctx, req)
		return rc, "ffmpeg", err
	}
	return resp.Body, "proxy", nil
}

type ffmpegReader struct {
	io.ReadCloser
	cmd    *exec.Cmd
	stderr *bytes.Buffer
	log    *slog.Logger
	name   string
}

func (f *ffmpegReader) Close() error {
	f.ReadCloser.Close()
	if f.cmd.Process != nil {
		f.cmd.Process.Kill()
	}
	f.cmd.Wait()
	if msg := strings.TrimSpace(f.stderr.String()); msg != "" {
		f.log.Debug("ffmpeg output", "channel", f.name, "stderr", redact.URL(msg))
	}
	return nil
}

func (m *Manager) openFFmpeg(ctx context.Context, req Request) (io.ReadCloser, error) {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	args = append(args, ident.FFmpegArgs(req.UserAgent)...)
	if strings.HasPrefix(req.URL, "http") {
		args = append(args, "-reconnect", "1", "-reconnect_streamed", "1", "-reconnect_delay_max", "2")
	}
	args = append(args, "-i", req.URL, "-map", "0", "-c", "copy", "-f", "mpegts", "pipe:1")
	cmd := exec.CommandContext(ctx, m.FFmpeg, args...)
	hideWindow(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &limitedBuffer{buf: &stderr, max: 8 << 10}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &ffmpegReader{ReadCloser: out, cmd: cmd, stderr: &stderr, log: m.Log, name: req.Name}, nil
}

// Transcode plays src to w through ffmpeg, copying the video and converting
// the audio to stereo AAC, so browsers can play channels with AC-3 or MP2
// audio. src should be this instance's own /stream URL, so the preview shares
// the channel's upstream instead of opening another provider connection.
func (m *Manager) Transcode(w http.ResponseWriter, r *http.Request, name, src string) {
	if !m.ffmpegOK {
		http.Error(w, "ffmpeg is not installed", http.StatusNotImplemented)
		return
	}
	cmd := exec.CommandContext(r.Context(), m.FFmpeg, "-hide_banner", "-loglevel", "error", "-nostdin",
		"-i", src, "-map", "0:v:0?", "-map", "0:a:0?", "-c:v", "copy", "-c:a", "aac", "-ac", "2", "-b:a", "192k",
		"-f", "mpegts", "pipe:1")
	hideWindow(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &limitedBuffer{buf: &stderr, max: 8 << 10}
	out, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	m.Log.Info("preview transcode started", "channel", name)
	buf := make([]byte, 64<<10)
	n, err := io.ReadAtLeast(out, buf, 1)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = "no data from stream"
		}
		m.Log.Warn("preview transcode failed", "channel", name, "err", redact.URL(msg))
		http.Error(w, "transcode failed: "+redact.URL(msg), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "no-store")
	rc := http.NewResponseController(w)
	for {
		if _, werr := w.Write(buf[:n]); werr != nil {
			break
		}
		rc.Flush()
		if n, err = out.Read(buf); err != nil {
			break
		}
	}
	m.Log.Info("preview transcode stopped", "channel", name)
}

type limitedBuffer struct {
	mu  sync.Mutex
	buf *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if room := l.max - l.buf.Len(); room > 0 {
		l.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// ---- status ----

type ClientInfo struct {
	Remote string    `json:"remote"`
	Since  time.Time `json:"since"`
}

type Info struct {
	ChannelID int64        `json:"channel_id"`
	Name      string       `json:"name"`
	SourceID  int64        `json:"source_id"`
	Mode      string       `json:"mode"`
	Started   time.Time    `json:"started"`
	Bytes     int64        `json:"bytes"`
	Clients   []ClientInfo `json:"clients"`
}

func (m *Manager) Sessions() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Info, 0, len(m.sessions))
	for _, s := range m.sessions {
		info := Info{ChannelID: s.req.ChannelID, Name: s.req.Name, SourceID: s.req.SourceID,
			Mode: s.mode.Load().(string), Started: s.started, Bytes: s.bytes.Load()}
		s.mu.Lock()
		for c := range s.clients {
			info.Clients = append(info.Clients, ClientInfo{Remote: c.remote, Since: c.since})
		}
		s.mu.Unlock()
		out = append(out, info)
	}
	return out
}

// Active returns the number of upstream connections per source.
func (m *Manager) Active() map[int64]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[int64]int, len(m.active))
	for k, v := range m.active {
		out[k] = v
	}
	return out
}

// Stop kills one channel's upstream and disconnects its clients.
func (m *Manager) Stop(channelID int64) bool {
	m.mu.Lock()
	s := m.sessions[channelID]
	m.mu.Unlock()
	if s == nil {
		return false
	}
	s.end()
	return true
}
