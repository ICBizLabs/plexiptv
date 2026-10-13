package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"

	"plexiptv/internal/app"
	"plexiptv/internal/catalog"
	"plexiptv/internal/ident"
	"plexiptv/internal/store"
)

// ---- status ----

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	counts := s.a.Cat.DeviceStats()
	type dev struct {
		store.Device
		Channels int `json:"channels"`
		Sent     int `json:"sent"`
	}
	var devs []dev
	for _, d := range s.a.Cat.Devices() {
		devs = append(devs, dev{d, counts[d.ID].Enabled, counts[d.ID].Sent})
	}
	var rulesErr string
	if err := s.a.Cat.RulesError(); err != nil {
		rulesErr = err.Error()
	}
	writeJSON(w, map[string]any{
		"devices":     devs,
		"web_port":    s.a.WebPort(),
		"host":        s.a.DisplayHost(),
		"ffmpeg":      s.a.Streams.FFmpegAvailable(),
		"streams":     len(s.a.Streams.Sessions()),
		"rules_error": rulesErr,
		"refreshing":  s.a.Refreshing(),
	})
}

func (s *Server) getLogs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.logs.Lines())
}

// ---- sources ----

type sourceOut struct {
	store.Source
	HasPassword bool `json:"has_password"`
	Refreshing  bool `json:"refreshing"`
	Active      int  `json:"active_streams"`
}

func (s *Server) listSources(w http.ResponseWriter, r *http.Request) {
	srcs, err := s.a.St.Sources(r.Context())
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	busy := s.a.Refreshing()
	active := s.a.Streams.Active()
	out := make([]sourceOut, 0, len(srcs))
	for _, src := range srcs {
		has := src.Password != ""
		src.Password = ""
		out = append(out, sourceOut{Source: src, HasPassword: has, Refreshing: busy[src.ID], Active: active[src.ID]})
	}
	writeJSON(w, out)
}

func (s *Server) saveSource(w http.ResponseWriter, r *http.Request) {
	var in store.Source
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	in.ID = pathID(r)
	in.Name = strings.TrimSpace(in.Name)
	in.URL = strings.TrimSpace(in.URL)
	in.Username = strings.TrimSpace(in.Username)
	switch in.Kind {
	case "xtream":
		if in.Username == "" {
			writeErr(w, 400, errors.New("username is required for Xtream sources"))
			return
		}
	case "m3u", "epg":
	default:
		writeErr(w, 400, errors.New("kind must be xtream, m3u or epg"))
		return
	}
	switch in.StreamMode {
	case "":
		in.StreamMode = "auto"
	case "auto", "proxy", "ffmpeg", "redirect":
	default:
		writeErr(w, 400, errors.New("invalid stream mode"))
		return
	}
	if in.URL == "" {
		writeErr(w, 400, errors.New("URL is required"))
		return
	}
	if in.Name == "" {
		in.Name = in.Kind
	}
	isNew := in.ID == 0
	if !isNew && in.Password == "" {
		old, err := s.a.St.Source(r.Context(), in.ID)
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		in.Password = old.Password
	}
	if isNew {
		in.Enabled = true
	}
	if err := s.a.St.SaveSource(r.Context(), &in); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.a.Cat.ReloadMeta(r.Context())
	if isNew {
		go s.a.RefreshSource(context.Background(), in.ID)
	}
	writeJSON(w, map[string]int64{"id": in.ID})
}

func (s *Server) deleteSource(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	if err := s.a.St.DeleteSource(r.Context(), id); err != nil {
		writeErr(w, 500, err)
		return
	}
	os.Remove(s.a.Ref.EPGPath(id))
	if err := s.a.Cat.Load(r.Context()); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.a.InvalidateEPG()
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) refreshSource(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	if s.a.Refreshing()[id] {
		writeErr(w, http.StatusConflict, app.ErrBusy)
		return
	}
	go s.a.RefreshSource(context.Background(), id)
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]bool{"started": true})
}

// ---- channels ----

func channelQuery(r *http.Request) catalog.Query {
	q := r.URL.Query()
	return catalog.Query{
		Search: q.Get("search"), Group: q.Get("group"), SourceID: qInt(r, "source"), DeviceID: qInt(r, "device"),
		Status: q.Get("status"), Sort: q.Get("sort"), Offset: int(qInt(r, "offset")), Limit: int(qInt(r, "limit")),
	}
}

func (s *Server) listChannels(w http.ResponseWriter, r *http.Request) {
	rows, total := s.a.Cat.Query(channelQuery(r))
	writeJSON(w, map[string]any{"rows": rows, "total": total})
}

type patchBody struct {
	IDs []int64 `json:"ids"`
	// AllMatching applies the patch to every channel matching the query
	// string of this request instead of explicit ids.
	AllMatching bool          `json:"all_matching"`
	Patch       catalog.Patch `json:"patch"`
}

func (s *Server) patchChannels(w http.ResponseWriter, r *http.Request) {
	var b patchBody
	if err := readJSON(r, &b); err != nil {
		writeErr(w, 400, err)
		return
	}
	ids := b.IDs
	if b.AllMatching {
		ids = s.a.Cat.MatchIDs(channelQuery(r))
	}
	if err := s.a.Cat.Apply(r.Context(), ids, b.Patch); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, map[string]int{"updated": len(ids)})
}

func (s *Server) renumber(w http.ResponseWriter, r *http.Request) {
	var b struct {
		IDs   []int64 `json:"ids"`
		Start int     `json:"start"`
	}
	if err := readJSON(r, &b); err != nil || b.Start <= 0 {
		writeErr(w, 400, errors.New("start number must be positive"))
		return
	}
	if err := s.a.Cat.Renumber(r.Context(), b.IDs, b.Start); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, map[string]int{"updated": len(b.IDs)})
}

func (s *Server) listGroups(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.a.Cat.Groups())
}

// toggleGroup is the one-click group checkbox: it adds, flips or removes a
// "group equals X" rule rather than touching individual channels.
func (s *Server) toggleGroup(w http.ResponseWriter, r *http.Request) {
	var b struct {
		SourceID int64  `json:"source_id"`
		Group    string `json:"group"`
		Include  bool   `json:"include"`
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, 400, err)
		return
	}
	ctx := r.Context()
	rules, err := s.a.St.Rules(ctx)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	var existing *store.Rule
	for i := range rules {
		x := &rules[i]
		if x.Field == "group" && x.Op == "equals" && x.SourceID == b.SourceID && strings.EqualFold(x.Value, b.Group) {
			existing = x
		}
	}
	want := "exclude"
	if b.Include {
		want = "include"
	}
	switch {
	case existing != nil && existing.Action == want:
		existing.Enabled = true
		err = s.a.St.SaveRule(ctx, existing)
	case existing != nil:
		// Opposite rule exists: deleting it may be enough (e.g. un-excluding).
		if err = s.a.St.DeleteRule(ctx, existing.ID); err == nil {
			s.a.Cat.ReloadMeta(ctx)
			if s.groupIncluded(b.SourceID, b.Group) != b.Include {
				err = s.a.St.SaveRule(ctx, &store.Rule{Action: want, Field: "group", Op: "equals", Value: b.Group, SourceID: b.SourceID, Enabled: true})
			}
		}
	default:
		err = s.a.St.SaveRule(ctx, &store.Rule{Action: want, Field: "group", Op: "equals", Value: b.Group, SourceID: b.SourceID, Enabled: true})
	}
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	s.a.Cat.ReloadMeta(ctx)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) groupIncluded(sourceID int64, group string) bool {
	for _, g := range s.a.Cat.Groups() {
		if g.SourceID == sourceID && g.Name == group {
			return g.Enabled > 0
		}
	}
	return false
}

// ---- rules ----

func (s *Server) listRules(w http.ResponseWriter, r *http.Request) {
	rules, err := s.a.St.Rules(r.Context())
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if rules == nil {
		rules = []store.Rule{}
	}
	var rerr string
	if err := s.a.Cat.RulesError(); err != nil {
		rerr = err.Error()
	}
	writeJSON(w, map[string]any{"rules": rules, "counts": s.a.Cat.RuleCounts(), "error": rerr})
}

func (s *Server) saveRule(w http.ResponseWriter, r *http.Request) {
	var in store.Rule
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	in.ID = pathID(r)
	if in.Op == "regex" {
		if _, err := regexp.Compile(in.Value); err != nil {
			writeErr(w, 400, fmt.Errorf("invalid regex: %v", err))
			return
		}
	}
	if _, err := s.a.Cat.PreviewRule(in); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.a.St.SaveRule(r.Context(), &in); err != nil {
		writeErr(w, 500, err)
		return
	}
	if err := s.a.Cat.ReloadMeta(r.Context()); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, map[string]int64{"id": in.ID})
}

func (s *Server) deleteRule(w http.ResponseWriter, r *http.Request) {
	if err := s.a.St.DeleteRule(r.Context(), pathID(r)); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.a.Cat.ReloadMeta(r.Context())
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) orderRules(w http.ResponseWriter, r *http.Request) {
	var b struct {
		IDs []int64 `json:"ids"`
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.a.St.ReorderRules(r.Context(), b.IDs); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.a.Cat.ReloadMeta(r.Context())
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) previewRule(w http.ResponseWriter, r *http.Request) {
	var in store.Rule
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	p, err := s.a.Cat.PreviewRule(in)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, p)
}

// ---- devices ----

func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	counts := s.a.Cat.DeviceStats()
	type dev struct {
		store.Device
		Channels int  `json:"channels"`
		Sent     int  `json:"sent"`
		WebPort  bool `json:"web_port"`
	}
	out := []dev{}
	for _, d := range s.a.Cat.Devices() {
		out = append(out, dev{d, counts[d.ID].Enabled, counts[d.ID].Sent, d.Port == s.a.WebPort()})
	}
	writeJSON(w, out)
}

func (s *Server) saveDevice(w http.ResponseWriter, r *http.Request) {
	var in store.Device
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	in.ID = pathID(r)
	devs := s.a.Cat.Devices()
	if in.Port <= 0 || in.Port > 65535 {
		writeErr(w, 400, errors.New("port must be 1-65535"))
		return
	}
	for _, d := range devs {
		if d.Port == in.Port && d.ID != in.ID {
			writeErr(w, 400, fmt.Errorf("port %d is already used by %s", in.Port, d.Name))
			return
		}
		if d.ID == in.ID && d.Port == s.a.WebPort() && in.Port != d.Port {
			writeErr(w, 400, errors.New("the default device must stay on the web panel port"))
			return
		}
	}
	if in.Tuners <= 0 {
		in.Tuners = 2
	}
	if in.MaxChannels < 0 {
		in.MaxChannels = 0
	}
	if in.Name == "" {
		in.Name = fmt.Sprintf("plexiptv %d", len(devs)+1)
	}
	if in.ID == 0 {
		in.DeviceID = app.RandomDeviceID()
		if in.StartNumber <= 0 {
			hi := 0
			for _, d := range devs {
				hi = max(hi, d.StartNumber)
			}
			in.StartNumber = hi + 1000
		}
	}
	if err := s.a.St.SaveDevice(r.Context(), &in); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.a.Cat.ReloadMeta(r.Context())
	writeJSON(w, map[string]int64{"id": in.ID})
}

func (s *Server) deleteDevice(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	for _, d := range s.a.Cat.Devices() {
		if d.ID == id && d.Port == s.a.WebPort() {
			writeErr(w, 400, errors.New("the default device cannot be deleted"))
			return
		}
	}
	if err := s.a.St.DeleteDevice(r.Context(), id); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.a.Cat.ReloadMeta(r.Context())
	writeJSON(w, map[string]bool{"ok": true})
}

// ---- EPG ----

func (s *Server) epgSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	all, err := s.a.St.EPGChannels(r.Context())
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	nq := normalize(q)
	type hit struct {
		store.EPGChannel
		score int
	}
	var hits []hit
	for _, c := range all {
		ln, li := strings.ToLower(c.Name), strings.ToLower(c.ID)
		score := 0
		switch {
		case q == "":
			score = 1
		case li == q || ln == q:
			score = 100
		case nq != "" && normalize(c.Name) == nq:
			score = 90
		case strings.HasPrefix(ln, q) || strings.HasPrefix(li, q):
			score = 50
		case strings.Contains(ln, q) || strings.Contains(li, q):
			score = 20
		case nq != "" && strings.Contains(normalize(c.Name), nq):
			score = 10
		}
		if score > 0 {
			hits = append(hits, hit{c, score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	out := []store.EPGChannel{}
	for i := 0; i < len(hits) && i < 50; i++ {
		out = append(out, hits[i].EPGChannel)
	}
	writeJSON(w, out)
}

var (
	reCountry  = regexp.MustCompile(`^[a-z]{2,3}\s*[:|\-]\s*`)
	reQuality  = regexp.MustCompile(`\b(uhd|fhd|hd|sd|4k|8k|hevc|h265|h\.265|raw|backup|vip|60fps|50fps|1080p|720p)\b`)
	reNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)
)

// normalize reduces a channel name to its core for EPG matching:
// "US: CNN HD" and "CNN" both become "cnn".
func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = reCountry.ReplaceAllString(s, "")
	s = strings.NewReplacer("+", "plus", "&", "and").Replace(s)
	s = reQuality.ReplaceAllString(s, "")
	return reNonAlnum.ReplaceAllString(s, "")
}

// epgAutomatch fills the EPG id of enabled channels that have none (or whose
// id isn't in any loaded guide) by matching normalised names.
func (s *Server) epgAutomatch(w http.ResponseWriter, r *http.Request) {
	all, err := s.a.St.EPGChannels(r.Context())
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	known := make(map[string]bool, len(all))
	byName := make(map[string]string, len(all))
	for _, c := range all {
		known[c.ID] = true
		if n := normalize(c.Name); n != "" {
			if _, ok := byName[n]; !ok {
				byName[n] = c.ID
			}
		}
		if n := normalize(strings.SplitN(c.ID, ".", 2)[0]); n != "" {
			if _, ok := byName[n]; !ok {
				byName[n] = c.ID
			}
		}
	}
	set := map[int64]string{}
	for _, e := range s.a.Cat.Lineup(0) {
		if e.EPGID != "" && known[e.EPGID] {
			continue
		}
		if id, ok := byName[normalize(e.Name)]; ok {
			set[e.ID] = id
		}
	}
	if err := s.a.Cat.SetEPGIDs(r.Context(), set); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, map[string]int{"matched": len(set)})
}

// ---- streams ----

func (s *Server) listStreams(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.a.Streams.Sessions())
}

func (s *Server) stopStream(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]bool{"stopped": s.a.Streams.Stop(pathID(r))})
}

// ---- settings ----

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	dummy, _ := s.a.St.GetSetting(r.Context(), "dummy_epg")
	writeJSON(w, map[string]any{"dummy_epg": dummy != "0", "data_dir": s.a.Cfg.DataDir, "ffmpeg": s.a.Streams.FFmpegAvailable(),
		"user_agent": ident.Default(), "ua_presets": ident.Presets})
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var b struct {
		DummyEPG    *bool   `json:"dummy_epg"`
		NewPassword string  `json:"new_password"`
		UserAgent   *string `json:"user_agent"`
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, 400, err)
		return
	}
	ctx := r.Context()
	if b.DummyEPG != nil {
		v := "1"
		if !*b.DummyEPG {
			v = "0"
		}
		s.a.St.SetSetting(ctx, "dummy_epg", v)
		s.a.InvalidateEPG()
	}
	if b.UserAgent != nil {
		ua := strings.TrimSpace(*b.UserAgent)
		if strings.ContainsAny(ua, "\r\n") {
			writeErr(w, 400, errors.New("invalid User-Agent"))
			return
		}
		if err := s.a.St.SetSetting(ctx, "user_agent", ua); err != nil {
			writeErr(w, 500, err)
			return
		}
		ident.SetDefault(ua)
	}
	if b.NewPassword != "" {
		if len(b.NewPassword) < 4 {
			writeErr(w, 400, errors.New("password must be at least 4 characters"))
			return
		}
		h, err := bcryptHash(b.NewPassword)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		if err := s.setHash(ctx, h); err != nil {
			writeErr(w, 500, err)
			return
		}
		s.startSession(w, r) // other sessions are now invalid; keep this one
	}
	writeJSON(w, map[string]bool{"ok": true})
}
