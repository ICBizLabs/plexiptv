// Package catalog keeps the live channel list, rules and overrides in memory
// and computes the lineup. The database is the source of truth; the catalog
// is rebuilt from it after imports and updated in place for edits, so the UI
// can search and filter 50k+ channels without hitting SQLite.
package catalog

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"plexiptv/internal/filter"
	"plexiptv/internal/store"
)

type channel struct {
	store.Channel
	f filter.Fields
}

type state struct {
	enabled  bool
	manual   bool
	capped   bool // enabled, but beyond its tuner's channel limit: not sent to Plex
	deviceID int64
	ruleID   int64
}

// Entry is a channel as it appears in a lineup.
type Entry struct {
	ID       int64
	SourceID int64
	ExtKey   string
	URL      string
	Number   int
	Name     string
	Logo     string
	EPGID    string
	DeviceID int64
	Mode     string
	Enabled  bool
}

type Catalog struct {
	st *store.Store

	mu       sync.RWMutex
	chans    []channel
	byID     map[int64]int
	ov       map[int64]store.Override
	rules    []filter.Rule
	rulesErr error
	devices  []store.Device
	sources  map[int64]store.Source
	state    []state
	version  int64

	listeners []func()
}

func New(st *store.Store) *Catalog {
	return &Catalog{st: st, byID: map[int64]int{}, ov: map[int64]store.Override{}, sources: map[int64]store.Source{}}
}

// OnChange registers a callback run (in a goroutine) after the lineup changes.
func (c *Catalog) OnChange(fn func()) {
	c.mu.Lock()
	c.listeners = append(c.listeners, fn)
	c.mu.Unlock()
}

func (c *Catalog) Version() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.version
}

// Load (re)reads everything from the database.
func (c *Catalog) Load(ctx context.Context) error {
	chans, err := c.st.AllChannels(ctx)
	if err != nil {
		return err
	}
	ov, err := c.st.Overrides(ctx)
	if err != nil {
		return err
	}
	rules, err := c.st.Rules(ctx)
	if err != nil {
		return err
	}
	devs, err := c.st.Devices(ctx)
	if err != nil {
		return err
	}
	srcs, err := c.st.Sources(ctx)
	if err != nil {
		return err
	}
	list := make([]channel, len(chans))
	byID := make(map[int64]int, len(chans))
	for i, ch := range chans {
		list[i] = channel{Channel: ch, f: filter.NewFields(ch.SourceID, ch.Name, ch.Group, ch.TvgID)}
		byID[ch.ID] = i
	}
	compiled, rerr := filter.Compile(rules)
	smap := make(map[int64]store.Source, len(srcs))
	for _, s := range srcs {
		smap[s.ID] = s
	}

	c.mu.Lock()
	c.chans, c.byID, c.ov, c.rules, c.rulesErr, c.devices, c.sources = list, byID, ov, compiled, rerr, devs, smap
	err = c.computeLocked(ctx)
	c.mu.Unlock()
	c.notify()
	return err
}

// ReloadMeta reloads rules, devices and sources (cheap) and recomputes.
func (c *Catalog) ReloadMeta(ctx context.Context) error {
	rules, err := c.st.Rules(ctx)
	if err != nil {
		return err
	}
	devs, err := c.st.Devices(ctx)
	if err != nil {
		return err
	}
	srcs, err := c.st.Sources(ctx)
	if err != nil {
		return err
	}
	compiled, rerr := filter.Compile(rules)
	smap := make(map[int64]store.Source, len(srcs))
	for _, s := range srcs {
		smap[s.ID] = s
	}
	c.mu.Lock()
	c.rules, c.rulesErr, c.devices, c.sources = compiled, rerr, devs, smap
	err = c.computeLocked(ctx)
	c.mu.Unlock()
	c.notify()
	return err
}

func (c *Catalog) RulesError() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.rulesErr
}

func (c *Catalog) notify() {
	c.mu.RLock()
	ls := append([]func(){}, c.listeners...)
	c.mu.RUnlock()
	for _, fn := range ls {
		go fn()
	}
}

func (c *Catalog) defaultDevice() int64 {
	if len(c.devices) == 0 {
		return 0
	}
	return c.devices[0].ID
}

func (c *Catalog) deviceExists(id int64) bool {
	for _, d := range c.devices {
		if d.ID == id {
			return true
		}
	}
	return false
}

// computeLocked evaluates rules for every channel and gives newly enabled
// channels a number. Numbers are persisted and never taken from another
// channel, so they stay stable across refreshes and enable/disable toggles.
func (c *Catalog) computeLocked(ctx context.Context) error {
	st := make([]state, len(c.chans))
	def := c.defaultDevice()
	for i := range c.chans {
		ch := &c.chans[i]
		res := filter.Evaluate(c.rules, &ch.f)
		s := state{enabled: res.Include, deviceID: res.DeviceID, ruleID: res.RuleID}
		o, hasOv := c.ov[ch.ID]
		if hasOv && o.Enabled != nil {
			s.enabled, s.manual = *o.Enabled, true
		}
		if hasOv && o.DeviceID != 0 {
			s.deviceID = o.DeviceID
		}
		if s.deviceID == 0 || !c.deviceExists(s.deviceID) {
			s.deviceID = def
		}
		if !ch.Present {
			s.enabled = false
		}
		st[i] = s
	}

	used := make(map[int]bool, len(c.ov))
	for _, o := range c.ov {
		if o.Number > 0 {
			used[o.Number] = true
		}
	}
	cursor := map[int64]int{}
	for _, d := range c.devices {
		cursor[d.ID] = max(d.StartNumber, 1)
	}
	var changed []store.Override
	for i := range c.chans {
		if !st[i].enabled {
			continue
		}
		id := c.chans[i].ID
		o := c.ov[id]
		if o.Number > 0 {
			continue
		}
		n := cursor[st[i].deviceID]
		if n == 0 {
			n = 1
		}
		for used[n] {
			n++
		}
		used[n] = true
		cursor[st[i].deviceID] = n + 1
		o.ChannelID = id
		o.Number = n
		c.ov[id] = o
		changed = append(changed, o)
	}
	c.applyCapsLocked(st)
	c.state = st
	c.version++
	if len(changed) > 0 {
		if err := c.st.SaveOverrides(ctx, changed); err != nil {
			return fmt.Errorf("save channel numbers: %w", err)
		}
	}
	return nil
}

// applyCapsLocked marks enabled channels beyond each tuner's MaxChannels
// (lowest numbers win). Plex stalls on huge lineups, so the overflow is
// held back rather than sent.
func (c *Catalog) applyCapsLocked(st []state) {
	limit := map[int64]int{}
	for _, d := range c.devices {
		limit[d.ID] = d.MaxChannels
	}
	byDev := map[int64][]int{}
	for i := range st {
		if st[i].enabled && limit[st[i].deviceID] > 0 {
			byDev[st[i].deviceID] = append(byDev[st[i].deviceID], i)
		}
	}
	for dev, idx := range byDev {
		max := limit[dev]
		if len(idx) <= max {
			continue
		}
		sort.Slice(idx, func(a, b int) bool {
			return c.ov[c.chans[idx[a]].ID].Number < c.ov[c.chans[idx[b]].ID].Number
		})
		for _, i := range idx[max:] {
			st[i].capped = true
		}
	}
}

func (c *Catalog) entryLocked(i int) Entry {
	ch := &c.chans[i]
	o := c.ov[ch.ID]
	e := Entry{
		ID: ch.ID, SourceID: ch.SourceID, ExtKey: ch.ExtKey, URL: ch.URL, Number: o.Number,
		Name: ch.Name, Logo: ch.Logo, EPGID: ch.TvgID, DeviceID: c.state[i].deviceID, Mode: o.Mode,
		Enabled: c.state[i].enabled,
	}
	if o.Name != "" {
		e.Name = o.Name
	}
	if o.Logo != "" {
		e.Logo = o.Logo
	}
	if o.EPGID != "" {
		e.EPGID = o.EPGID
	}
	return e
}

// Lineup returns the channels sent to Plex for a device (0 = all devices),
// by number: enabled and within the tuner's channel limit.
func (c *Catalog) Lineup(deviceID int64) []Entry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []Entry
	for i := range c.chans {
		if !c.state[i].enabled || c.state[i].capped || (deviceID != 0 && c.state[i].deviceID != deviceID) {
			continue
		}
		out = append(out, c.entryLocked(i))
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Number < out[b].Number })
	return out
}

// Get returns a present channel by id (enabled or not, so the UI can preview).
func (c *Catalog) Get(id int64) (Entry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	i, ok := c.byID[id]
	if !ok || !c.chans[i].Present {
		return Entry{}, false
	}
	return c.entryLocked(i), true
}

func (c *Catalog) Devices() []store.Device {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]store.Device(nil), c.devices...)
}

func (c *Catalog) Source(id int64) (store.Source, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.sources[id]
	return s, ok
}

// ---- UI queries ----

type Row struct {
	ID         int64  `json:"id"`
	SourceID   int64  `json:"source_id"`
	SourceName string `json:"source_name"`
	Name       string `json:"name"`
	OrigName   string `json:"orig_name"`
	Group      string `json:"group"`
	Logo       string `json:"logo"`
	TvgID      string `json:"tvg_id"`
	EPGID      string `json:"epg_id"`
	Number     int    `json:"number"`
	DeviceID   int64  `json:"device_id"`
	Enabled    bool   `json:"enabled"`
	Manual     bool   `json:"manual"`
	Capped     bool   `json:"capped"`
	RuleID     int64  `json:"rule_id"`
	Mode       string `json:"mode"`
}

type Query struct {
	Search   string
	Group    string
	SourceID int64
	DeviceID int64
	Status   string // all | enabled | disabled | manual | noepg | capped
	Sort     string // source | number | name
	Offset   int
	Limit    int
}

func (c *Catalog) rowLocked(i int) Row {
	ch := &c.chans[i]
	e := c.entryLocked(i)
	return Row{
		ID: ch.ID, SourceID: ch.SourceID, SourceName: c.sources[ch.SourceID].Name, Name: e.Name, OrigName: ch.Name,
		Group: ch.Group, Logo: e.Logo, TvgID: ch.TvgID, EPGID: e.EPGID, Number: e.Number, DeviceID: e.DeviceID,
		Enabled: e.Enabled, Manual: c.state[i].manual, Capped: c.state[i].capped, RuleID: c.state[i].ruleID, Mode: e.Mode,
	}
}

func (c *Catalog) matchQuery(i int, q *Query, terms []string) bool {
	ch := &c.chans[i]
	if !ch.Present {
		return false
	}
	if q.SourceID != 0 && ch.SourceID != q.SourceID {
		return false
	}
	if q.Group != "" && ch.Group != q.Group {
		return false
	}
	s := c.state[i]
	if q.DeviceID != 0 && (!s.enabled || s.deviceID != q.DeviceID) {
		return false
	}
	switch q.Status {
	case "enabled":
		if !s.enabled {
			return false
		}
	case "disabled":
		if s.enabled {
			return false
		}
	case "manual":
		if !s.manual {
			return false
		}
	case "capped":
		if !s.capped {
			return false
		}
	case "noepg":
		if !s.enabled || c.entryLocked(i).EPGID != "" {
			return false
		}
	}
	for _, t := range terms {
		if !strings.Contains(ch.f.LName, t) && !strings.Contains(ch.f.LGroup, t) && !strings.Contains(ch.f.LTvgID, t) {
			if o := c.ov[ch.ID]; o.Name == "" || !strings.Contains(strings.ToLower(o.Name), t) {
				return false
			}
		}
	}
	return true
}

func (c *Catalog) Query(q Query) ([]Row, int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	terms := strings.Fields(strings.ToLower(q.Search))
	idx := make([]int, 0, 1024)
	for i := range c.chans {
		if c.matchQuery(i, &q, terms) {
			idx = append(idx, i)
		}
	}
	switch q.Sort {
	case "number":
		sort.SliceStable(idx, func(a, b int) bool {
			na, nb := c.ov[c.chans[idx[a]].ID].Number, c.ov[c.chans[idx[b]].ID].Number
			ea, eb := c.state[idx[a]].enabled, c.state[idx[b]].enabled
			if ea != eb {
				return ea
			}
			return na < nb
		})
	case "name":
		sort.SliceStable(idx, func(a, b int) bool { return c.chans[idx[a]].f.LName < c.chans[idx[b]].f.LName })
	}
	total := len(idx)
	if q.Limit <= 0 {
		q.Limit = 100
	}
	start := min(max(q.Offset, 0), total)
	end := min(start+q.Limit, total)
	rows := make([]Row, 0, end-start)
	for _, i := range idx[start:end] {
		rows = append(rows, c.rowLocked(i))
	}
	return rows, total
}

// MatchIDs returns every channel id matching a query (for "select all results").
func (c *Catalog) MatchIDs(q Query) []int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	terms := strings.Fields(strings.ToLower(q.Search))
	var out []int64
	for i := range c.chans {
		if c.matchQuery(i, &q, terms) {
			out = append(out, c.chans[i].ID)
		}
	}
	return out
}

type Group struct {
	SourceID   int64  `json:"source_id"`
	SourceName string `json:"source_name"`
	Name       string `json:"name"`
	Total      int    `json:"total"`
	Enabled    int    `json:"enabled"`
}

func (c *Catalog) Groups() []Group {
	c.mu.RLock()
	defer c.mu.RUnlock()
	type key struct {
		src  int64
		name string
	}
	pos := map[key]int{}
	var out []Group
	for i := range c.chans {
		ch := &c.chans[i]
		if !ch.Present {
			continue
		}
		k := key{ch.SourceID, ch.Group}
		p, ok := pos[k]
		if !ok {
			p = len(out)
			pos[k] = p
			out = append(out, Group{SourceID: ch.SourceID, SourceName: c.sources[ch.SourceID].Name, Name: ch.Group})
		}
		out[p].Total++
		if c.state[i].enabled {
			out[p].Enabled++
		}
	}
	return out
}

type RulePreview struct {
	Matches int      `json:"matches"`
	Sample  []string `json:"sample"`
}

// PreviewRule counts how many present channels a (possibly unsaved) rule matches.
func (c *Catalog) PreviewRule(r store.Rule) (RulePreview, error) {
	r.Enabled = true
	cr, err := filter.CompileOne(r)
	if err != nil {
		return RulePreview{}, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	var p RulePreview
	for i := range c.chans {
		ch := &c.chans[i]
		if ch.Present && cr.Match(&ch.f) {
			p.Matches++
			if len(p.Sample) < 12 {
				p.Sample = append(p.Sample, ch.Name+"  ["+ch.Group+"]")
			}
		}
	}
	return p, nil
}

// RuleCounts reports, per rule id, how many channels each rule matches.
func (c *Catalog) RuleCounts() map[int64]int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := map[int64]int{}
	for ri := range c.rules {
		r := &c.rules[ri]
		n := 0
		for i := range c.chans {
			if c.chans[i].Present && r.Match(&c.chans[i].f) {
				n++
			}
		}
		out[r.ID] = n
	}
	return out
}

// DeviceStat counts a tuner's channels: Enabled by rules/overrides, and
// Sent to Plex after the channel limit.
type DeviceStat struct {
	Enabled int `json:"enabled"`
	Sent    int `json:"sent"`
}

func (c *Catalog) DeviceStats() map[int64]DeviceStat {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := map[int64]DeviceStat{}
	for i := range c.state {
		if c.state[i].enabled {
			d := out[c.state[i].deviceID]
			d.Enabled++
			if !c.state[i].capped {
				d.Sent++
			}
			out[c.state[i].deviceID] = d
		}
	}
	return out
}

// ---- edits ----

// Patch is a partial update applied to many channels. Nil fields are untouched.
type Patch struct {
	Enabled  *string `json:"enabled"` // "on" | "off" | "rules"
	DeviceID *int64  `json:"device_id"`
	Number   *int    `json:"number"`
	Name     *string `json:"name"`
	Logo     *string `json:"logo"`
	EPGID    *string `json:"epg_id"`
	Mode     *string `json:"mode"`
}

func (c *Catalog) Apply(ctx context.Context, ids []int64, p Patch) error {
	c.mu.Lock()
	var list []store.Override
	for _, id := range ids {
		if _, ok := c.byID[id]; !ok {
			continue
		}
		o := c.ov[id]
		o.ChannelID = id
		if p.Enabled != nil {
			switch *p.Enabled {
			case "on":
				v := true
				o.Enabled = &v
			case "off":
				v := false
				o.Enabled = &v
			default:
				o.Enabled = nil
			}
		}
		if p.DeviceID != nil {
			o.DeviceID = *p.DeviceID
		}
		if p.Number != nil {
			o.Number = *p.Number
		}
		if p.Name != nil {
			o.Name = strings.TrimSpace(*p.Name)
		}
		if p.Logo != nil {
			o.Logo = strings.TrimSpace(*p.Logo)
		}
		if p.EPGID != nil {
			o.EPGID = strings.TrimSpace(*p.EPGID)
		}
		if p.Mode != nil {
			o.Mode = *p.Mode
		}
		list = append(list, o)
	}
	// A manually typed number takes over: whoever held it gets a new one.
	if p.Number != nil && *p.Number > 0 {
		sel := idSet(ids)
		for oid, o := range c.ov {
			if o.Number == *p.Number && !sel[oid] {
				o.Number = 0
				c.ov[oid] = o
				list = append(list, o)
			}
		}
	}
	err := c.saveLocked(ctx, list)
	c.mu.Unlock()
	c.notify()
	return err
}

// Renumber gives the channels consecutive numbers from start, in the order given.
func (c *Catalog) Renumber(ctx context.Context, ids []int64, start int) error {
	c.mu.Lock()
	target := map[int]bool{}
	var list []store.Override
	for i, id := range ids {
		if _, ok := c.byID[id]; !ok {
			continue
		}
		o := c.ov[id]
		o.ChannelID = id
		o.Number = start + i
		target[o.Number] = true
		list = append(list, o)
	}
	sel := idSet(ids)
	for oid, o := range c.ov {
		if target[o.Number] && !sel[oid] {
			o.Number = 0
			list = append(list, o)
		}
	}
	err := c.saveLocked(ctx, list)
	c.mu.Unlock()
	c.notify()
	return err
}

func (c *Catalog) saveLocked(ctx context.Context, list []store.Override) error {
	if err := c.st.SaveOverrides(ctx, list); err != nil {
		return err
	}
	for _, o := range list {
		if o.IsZero() {
			delete(c.ov, o.ChannelID)
		} else {
			c.ov[o.ChannelID] = o
		}
	}
	return c.computeLocked(ctx)
}

func idSet(ids []int64) map[int64]bool {
	m := make(map[int64]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// SetEPGIDs assigns per-channel EPG ids in one batch.
func (c *Catalog) SetEPGIDs(ctx context.Context, ids map[int64]string) error {
	if len(ids) == 0 {
		return nil
	}
	c.mu.Lock()
	list := make([]store.Override, 0, len(ids))
	for id, epgID := range ids {
		if _, ok := c.byID[id]; !ok {
			continue
		}
		o := c.ov[id]
		o.ChannelID = id
		o.EPGID = epgID
		list = append(list, o)
	}
	err := c.saveLocked(ctx, list)
	c.mu.Unlock()
	c.notify()
	return err
}
