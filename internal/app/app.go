// Package app wires the store, catalog, refresher, stream manager and the
// per-device HTTP listeners together.
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"plexiptv/internal/catalog"
	"plexiptv/internal/config"
	"plexiptv/internal/epg"
	"plexiptv/internal/hdhr"
	"plexiptv/internal/source"
	"plexiptv/internal/store"
	"plexiptv/internal/stream"
)

type App struct {
	Cfg     config.Config
	Log     *slog.Logger
	St      *store.Store
	Cat     *catalog.Catalog
	Ref     *source.Refresher
	Streams *stream.Manager

	webPort int
	epgGen  atomic.Int64

	refreshMu  sync.Mutex
	refreshing map[int64]bool

	xmltvMu   sync.Mutex
	xmltvKey  string
	xmltvCur  atomic.Value // string: path of the latest complete guide
	xmltvKick chan struct{}

	srvMu   sync.Mutex
	servers map[int]*http.Server
	webMux  http.Handler
	runCtx  context.Context
}

func New(cfg config.Config, log *slog.Logger) (*App, error) {
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	_, portStr, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen %q: %w", cfg.Listen, err)
	}
	webPort, _ := strconv.Atoi(portStr)
	a := &App{
		Cfg: cfg, Log: log, St: st, Cat: catalog.New(st), webPort: webPort,
		refreshing: map[int64]bool{}, servers: map[int]*http.Server{}, xmltvKick: make(chan struct{}, 1),
		Ref: &source.Refresher{
			St: st, DataDir: cfg.DataDir, Log: log,
			HTTP: &http.Client{Timeout: 15 * time.Minute, Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 60 * time.Second,
			}},
		},
		Streams: stream.NewManager(cfg.FFmpegPath, log),
	}
	ctx := context.Background()
	if err := a.ensureDevice(ctx); err != nil {
		return nil, err
	}
	if err := a.Cat.Load(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *App) WebPort() int { return a.webPort }

func RandomDeviceID() string {
	b := make([]byte, 4)
	rand.Read(b)
	b[0] = b[0]&0x0f | 0x10 // avoid leading zero / all-F wildcard
	return strings.ToUpper(hex.EncodeToString(b))
}

// ensureDevice creates the default tuner on first run, and keeps a tuner on
// the web panel port if "listen" was changed in config.yaml since.
func (a *App) ensureDevice(ctx context.Context) error {
	devs, err := a.St.Devices(ctx)
	if err != nil {
		return err
	}
	if len(devs) == 0 {
		return a.St.SaveDevice(ctx, &store.Device{Name: "plexiptv", DeviceID: RandomDeviceID(), Port: a.webPort, Tuners: 4, StartNumber: 1000})
	}
	for _, d := range devs {
		if d.Port == a.webPort {
			return nil
		}
	}
	d := devs[0]
	a.Log.Info("moving default tuner to the web panel port", "tuner", d.Name, "from", d.Port, "to", a.webPort)
	d.Port = a.webPort
	return a.St.SaveDevice(ctx, &d)
}

// Run serves until ctx is cancelled. web is the panel/API handler mounted on the main port.
func (a *App) Run(ctx context.Context, web http.Handler) error {
	a.runCtx = ctx
	a.webMux = web
	a.Cat.OnChange(func() { a.syncDevices(); a.kickXMLTV() })
	go a.xmltvWorker(ctx)
	a.kickXMLTV()

	mainSrv := &http.Server{Addr: a.Cfg.Listen, Handler: a.deviceMux(a.webPort, web), ReadHeaderTimeout: 15 * time.Second}
	ln, err := net.Listen("tcp", a.Cfg.Listen)
	if err != nil {
		return fmt.Errorf("web panel: %w", err)
	}
	a.Log.Info("web panel listening", "url", fmt.Sprintf("http://%s:%d/", a.DisplayHost(), a.webPort))
	go mainSrv.Serve(ln)

	a.syncDevices()
	if a.Cfg.SSDP {
		(&hdhr.Announcer{Devices: a.Cat.Devices, Advertise: a.Cfg.AdvertiseHost, Log: a.Log}).Run(ctx)
	}
	go a.scheduler(ctx)

	<-ctx.Done()
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mainSrv.Shutdown(shut)
	a.srvMu.Lock()
	for _, s := range a.servers {
		s.Shutdown(shut)
	}
	a.srvMu.Unlock()
	return a.St.Close()
}

// DisplayHost is the LAN address shown to users (advertise_host or the outbound interface IP).
func (a *App) DisplayHost() string {
	if a.Cfg.AdvertiseHost != "" {
		return a.Cfg.AdvertiseHost
	}
	if c, err := net.Dial("udp4", "192.0.2.1:9"); err == nil { // no packets are sent
		defer c.Close()
		return c.LocalAddr().(*net.UDPAddr).IP.String()
	}
	return "localhost"
}

// deviceMux serves the HDHomeRun API for whichever device owns port, plus
// the stream, XMLTV and M3U endpoints. extra (the web UI) is mounted at "/".
func (a *App) deviceMux(port int, extra http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	h := &hdhr.Handlers{
		Advertise: a.Cfg.AdvertiseHost,
		Device: func() (store.Device, bool) {
			for _, d := range a.Cat.Devices() {
				if d.Port == port {
					return d, true
				}
			}
			return store.Device{}, false
		},
		Lineup: func(d store.Device, base string) []hdhr.LineupItem {
			entries := a.Cat.Lineup(d.ID)
			items := make([]hdhr.LineupItem, len(entries))
			for i, e := range entries {
				items[i] = hdhr.LineupItem{GuideNumber: hdhr.GuideNumber(e.Number), GuideName: e.Name,
					URL: fmt.Sprintf("%s/stream/%d", base, e.ID), HD: 1}
			}
			return items
		},
	}
	h.Register(mux)
	mux.HandleFunc("GET /stream/{id}", a.serveStream)
	mux.HandleFunc("HEAD /stream/{id}", a.serveStream)
	mux.HandleFunc("GET /xmltv.xml", a.serveXMLTV)
	mux.HandleFunc("GET /lineup.m3u", a.serveM3U)
	if extra != nil {
		mux.Handle("/", extra)
	}
	return mux
}

// syncDevices starts/stops listeners so every device port (other than the
// web port, which already serves the default device) is listening.
func (a *App) syncDevices() {
	if a.runCtx == nil {
		return
	}
	want := map[int]bool{}
	for _, d := range a.Cat.Devices() {
		if d.Port != a.webPort {
			want[d.Port] = true
		}
	}
	a.srvMu.Lock()
	defer a.srvMu.Unlock()
	for port, srv := range a.servers {
		if !want[port] {
			srv.Close()
			delete(a.servers, port)
			a.Log.Info("tuner listener stopped", "port", port)
		}
	}
	for port := range want {
		if _, ok := a.servers[port]; ok {
			continue
		}
		addr := fmt.Sprintf(":%d", port)
		if host, _, err := net.SplitHostPort(a.Cfg.Listen); err == nil && host != "" {
			addr = net.JoinHostPort(host, strconv.Itoa(port))
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			a.Log.Error("cannot listen for tuner device", "port", port, "err", err)
			continue
		}
		srv := &http.Server{Handler: a.deviceMux(port, nil), ReadHeaderTimeout: 15 * time.Second}
		a.servers[port] = srv
		go srv.Serve(ln)
		a.Log.Info("tuner listening", "url", fmt.Sprintf("http://%s:%d/", a.DisplayHost(), port))
	}
}

func (a *App) serveStream(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimSuffix(r.PathValue("id"), ".ts"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	e, ok := a.Cat.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	src, ok := a.Cat.Source(e.SourceID)
	if !ok || !src.Enabled {
		http.Error(w, "source disabled", http.StatusNotFound)
		return
	}
	mode := e.Mode
	if mode == "" {
		mode = src.StreamMode
	}
	a.Streams.Serve(w, r, stream.Request{
		ChannelID: e.ID, SourceID: src.ID, Name: e.Name,
		URL: source.StreamURL(src, e.ExtKey, e.URL), UserAgent: src.UserAgent,
		Mode: mode, Limit: src.EffectiveConns(),
	})
}

func (a *App) serveM3U(w http.ResponseWriter, r *http.Request) {
	base := hdhr.BaseURL(r, a.Cfg.AdvertiseHost)
	w.Header().Set("Content-Type", "audio/x-mpegurl; charset=utf-8")
	fmt.Fprintf(w, "#EXTM3U url-tvg=\"%s/xmltv.xml\"\n", base)
	for _, e := range a.Cat.Lineup(0) {
		fmt.Fprintf(w, "#EXTINF:-1 tvg-id=\"%d\" tvg-chno=\"%d\" tvg-name=%q tvg-logo=%q,%s\n%s/stream/%d\n",
			e.Number, e.Number, e.Name, e.Logo, e.Name, base, e.ID)
	}
}

// ---- XMLTV ----
//
// The guide is rebuilt in the background (debounced) after lineup or EPG
// changes, so a Plex guide fetch never waits on generation. Each build goes
// to a new file and the pointer is swapped when it's complete: replacing a
// file that Plex is still downloading fails on Windows.

func (a *App) guideDir() string { return filepath.Join(a.Cfg.DataDir, "guide") }

func (a *App) currentXMLTV() string {
	p, _ := a.xmltvCur.Load().(string)
	return p
}

func (a *App) serveXMLTV(w http.ResponseWriter, r *http.Request) {
	p := a.currentXMLTV()
	if p == "" { // nothing built yet (first request right after startup)
		if err := a.buildXMLTV(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		p = a.currentXMLTV()
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	http.ServeFile(w, r, p)
}

// InvalidateEPG schedules a guide rebuild (after refreshes and setting changes).
func (a *App) InvalidateEPG() {
	a.epgGen.Add(1)
	a.kickXMLTV()
}

func (a *App) kickXMLTV() {
	select {
	case a.xmltvKick <- struct{}{}:
	default:
	}
}

func (a *App) xmltvWorker(ctx context.Context) {
	const quiet = 3 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.xmltvKick:
		}
		// Debounce: bulk edits fire many changes in a row.
		t := time.NewTimer(quiet)
	wait:
		for {
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-a.xmltvKick:
				t.Reset(quiet)
			case <-t.C:
				break wait
			}
		}
		a.buildXMLTV(ctx)
	}
}

func (a *App) buildXMLTV(ctx context.Context) error {
	a.xmltvMu.Lock()
	defer a.xmltvMu.Unlock()
	dummy, _ := a.St.GetSetting(ctx, "dummy_epg")
	key := fmt.Sprintf("%d/%d/%s", a.Cat.Version(), a.epgGen.Load(), dummy)
	if key == a.xmltvKey && a.currentXMLTV() != "" {
		return nil
	}
	if err := os.MkdirAll(a.guideDir(), 0o755); err != nil {
		return err
	}
	path := filepath.Join(a.guideDir(), fmt.Sprintf("xmltv-%d.xml", time.Now().UnixNano()))
	start := time.Now()
	if err := a.generateXMLTV(ctx, path, dummy != "0"); err != nil {
		os.Remove(path)
		a.Log.Error("xmltv generation failed", "err", err)
		return err
	}
	prev := a.currentXMLTV()
	a.xmltvCur.Store(path)
	a.xmltvKey = key
	a.Log.Info("xmltv generated", "took", time.Since(start).Round(time.Millisecond))
	// Keep the current and previous file (it may still be downloading);
	// anything older is removed. Removal of an open file fails on Windows
	// and is simply retried after the next build.
	if old, err := filepath.Glob(filepath.Join(a.guideDir(), "xmltv-*.xml")); err == nil {
		for _, f := range old {
			if f != path && f != prev {
				os.Remove(f)
			}
		}
	}
	return nil
}

func (a *App) generateXMLTV(ctx context.Context, path string, dummy bool) error {
	lineup := a.Cat.Lineup(0)
	epgChans, err := a.St.EPGChannels(ctx)
	if err != nil {
		return err
	}
	owner := make(map[string]int64, len(epgChans))
	for _, c := range epgChans {
		if _, ok := owner[c.ID]; !ok {
			owner[c.ID] = c.SourceID
		}
	}
	plan := epg.Plan{Map: map[string][]string{}, Now: time.Now()}
	for _, e := range lineup {
		id := strconv.Itoa(e.Number)
		oc := epg.OutChannel{ID: id, Name: e.Name, Logo: e.Logo}
		plan.Channels = append(plan.Channels, oc)
		if _, ok := owner[e.EPGID]; ok && e.EPGID != "" {
			plan.Map[e.EPGID] = append(plan.Map[e.EPGID], id)
		} else if dummy {
			plan.Dummy = append(plan.Dummy, oc)
		}
	}
	srcs, err := a.St.Sources(ctx)
	if err != nil {
		return err
	}
	for _, s := range srcs {
		p := a.Ref.EPGPath(s.ID)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		sid := s.ID
		plan.Files = append(plan.Files, epg.File{Path: p, Owns: func(id string) bool { return owner[id] == sid }})
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := epg.Generate(f, plan); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ---- refresh ----

var ErrBusy = errors.New("refresh already running for this source")

func (a *App) RefreshSource(ctx context.Context, id int64) (source.Result, error) {
	a.refreshMu.Lock()
	if a.refreshing[id] {
		a.refreshMu.Unlock()
		return source.Result{}, ErrBusy
	}
	a.refreshing[id] = true
	a.refreshMu.Unlock()
	defer func() {
		a.refreshMu.Lock()
		delete(a.refreshing, id)
		a.refreshMu.Unlock()
	}()

	src, err := a.St.Source(ctx, id)
	if err != nil {
		return source.Result{}, err
	}
	res, err := a.Ref.Refresh(ctx, src)
	if lerr := a.Cat.Load(context.Background()); lerr != nil {
		a.Log.Error("reload catalog", "err", lerr)
	}
	a.InvalidateEPG()
	return res, err
}

func (a *App) Refreshing() map[int64]bool {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	out := make(map[int64]bool, len(a.refreshing))
	for k, v := range a.refreshing {
		out[k] = v
	}
	return out
}

func (a *App) scheduler(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		srcs, err := a.St.Sources(ctx)
		if err == nil {
			now := time.Now().Unix()
			for _, s := range srcs {
				if !s.Enabled || s.RefreshHours <= 0 {
					continue
				}
				if s.LastRefresh+int64(s.RefreshHours)*3600 <= now {
					if _, err := a.RefreshSource(ctx, s.ID); err != nil && !errors.Is(err, ErrBusy) {
						a.Log.Warn("scheduled refresh failed", "source", s.Name, "err", err)
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
