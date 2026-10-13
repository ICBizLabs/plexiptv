package catalog

import (
	"context"
	"fmt"
	"testing"
	"time"

	"plexiptv/internal/store"
)

func importChannels(t *testing.T, st *store.Store, srcID int64, chans []store.Channel) {
	t.Helper()
	ctx := context.Background()
	im, err := st.BeginImport(ctx, srcID, time.Now().UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chans {
		if err := im.Add(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if err := im.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func numbers(c *Catalog) map[string]int {
	out := map[string]int{}
	for _, e := range c.Lineup(0) {
		out[e.Name] = e.Number
	}
	return out
}

func TestStableNumbering(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	src := store.Source{Name: "p", Kind: "m3u", URL: "x", Enabled: true}
	st.SaveSource(ctx, &src)
	st.SaveDevice(ctx, &store.Device{Name: "d", DeviceID: "11111111", Port: 1, Tuners: 2, StartNumber: 100})
	st.SaveRule(ctx, &store.Rule{Action: "include", Field: "group", Op: "equals", Value: "news", Enabled: true})

	importChannels(t, st, src.ID, []store.Channel{
		{ExtKey: "a", Name: "A", Group: "News"},
		{ExtKey: "b", Name: "B", Group: "News"},
		{ExtKey: "c", Name: "C", Group: "Sports"},
		{ExtKey: "d", Name: "D", Group: "News"},
	})
	c := New(st)
	if err := c.Load(ctx); err != nil {
		t.Fatal(err)
	}
	got := numbers(c)
	if fmt.Sprint(got) != fmt.Sprint(map[string]int{"A": 100, "B": 101, "D": 102}) {
		t.Fatalf("initial numbers %v", got)
	}

	// Provider reorders, drops B, adds E: existing numbers must not move.
	importChannels(t, st, src.ID, []store.Channel{
		{ExtKey: "e", Name: "E", Group: "News"},
		{ExtKey: "d", Name: "D", Group: "News"},
		{ExtKey: "a", Name: "A", Group: "News"},
	})
	if err := c.Load(ctx); err != nil {
		t.Fatal(err)
	}
	got = numbers(c)
	if got["A"] != 100 || got["D"] != 102 || got["E"] != 103 || got["B"] != 0 {
		t.Fatalf("after refresh %v", got)
	}

	// B comes back and keeps its old number.
	importChannels(t, st, src.ID, []store.Channel{
		{ExtKey: "a", Name: "A", Group: "News"}, {ExtKey: "b", Name: "B", Group: "News"},
		{ExtKey: "c", Name: "C", Group: "Sports"}, {ExtKey: "d", Name: "D", Group: "News"}, {ExtKey: "e", Name: "E", Group: "News"},
	})
	c.Load(ctx)
	if got = numbers(c); got["B"] != 101 {
		t.Fatalf("B should keep 101, got %v", got)
	}

	// Manual override beats the rule, and survives a reload from the DB.
	rows, _ := c.Query(Query{Search: "c"})
	on := "on"
	if err := c.Apply(ctx, []int64{rows[0].ID}, Patch{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	c2 := New(st)
	c2.Load(ctx)
	if got = numbers(c2); got["C"] == 0 {
		t.Fatalf("manual include lost: %v", got)
	}

	// Typing a number that's taken moves the previous holder.
	rowsA, _ := c2.Query(Query{Search: "a", Status: "enabled"})
	n := 101
	c2.Apply(ctx, []int64{rowsA[0].ID}, Patch{Number: &n})
	got = numbers(c2)
	if got["A"] != 101 || got["B"] == 101 || got["B"] == 0 {
		t.Fatalf("number takeover: %v", got)
	}
}

func TestTunerChannelLimit(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	src := store.Source{Name: "p", Kind: "m3u", URL: "x", Enabled: true}
	st.SaveSource(ctx, &src)
	dev := store.Device{Name: "d", DeviceID: "11111111", Port: 1, Tuners: 2, StartNumber: 1, MaxChannels: 2}
	st.SaveDevice(ctx, &dev)
	st.SaveRule(ctx, &store.Rule{Action: "include", Field: "any", Op: "contains", Value: "", Enabled: true})
	importChannels(t, st, src.ID, []store.Channel{
		{ExtKey: "a", Name: "A"}, {ExtKey: "b", Name: "B"}, {ExtKey: "c", Name: "C"},
	})
	c := New(st)
	if err := c.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if got := numbers(c); fmt.Sprint(got) != fmt.Sprint(map[string]int{"A": 1, "B": 2}) {
		t.Fatalf("lineup should hold the 2 lowest numbers, got %v", got)
	}
	if s := c.DeviceStats()[dev.ID]; s.Enabled != 3 || s.Sent != 2 {
		t.Fatalf("stats %+v", s)
	}
	rows, _ := c.Query(Query{Status: "capped"})
	if len(rows) != 1 || rows[0].Name != "C" || !rows[0].Capped {
		t.Fatalf("capped rows %+v", rows)
	}

	// Renumbering C below the others pushes B out instead.
	rowsC, _ := c.Query(Query{Search: "c"})
	if err := c.Renumber(ctx, []int64{rowsC[0].ID}, 1); err != nil {
		t.Fatal(err)
	}
	got := numbers(c)
	if got["C"] != 1 || len(got) != 2 {
		t.Fatalf("after renumber %v", got)
	}

	// Lifting the limit sends everything.
	dev.MaxChannels = 0
	st.SaveDevice(ctx, &dev)
	c.ReloadMeta(ctx)
	if got := numbers(c); len(got) != 3 {
		t.Fatalf("no limit should send all, got %v", got)
	}
}
