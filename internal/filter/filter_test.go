package filter

import (
	"testing"

	"plexiptv/internal/store"
)

func TestEvaluateOrder(t *testing.T) {
	rules, err := Compile([]store.Rule{
		{ID: 1, Action: "include", Field: "group", Op: "prefix", Value: "US |", Enabled: true},
		{ID: 2, Action: "exclude", Field: "name", Op: "regex", Value: `\b(4k|uhd)\b`, Enabled: true},
		{ID: 3, Action: "include", Field: "name", Op: "equals", Value: "us: espn 4k", DeviceID: 7, Enabled: true},
		{ID: 4, Action: "exclude", Field: "any", Op: "contains", Value: "x", Enabled: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, group string
		include     bool
		dev         int64
	}{
		{"US: CNN", "US | News", true, 0},
		{"US: HBO 4K", "US | Movies", false, 0},
		{"US: ESPN 4K", "US | Sports", true, 7},
		{"UK: BBC", "UK | General", false, 0},
	}
	for _, c := range cases {
		f := NewFields(1, c.name, c.group, "")
		got := Evaluate(rules, &f)
		if got.Include != c.include || got.DeviceID != c.dev {
			t.Errorf("%s: got %+v", c.name, got)
		}
	}
}

func TestBadRegex(t *testing.T) {
	rules, err := Compile([]store.Rule{
		{ID: 1, Action: "include", Field: "name", Op: "regex", Value: "(", Enabled: true},
		{ID: 2, Action: "include", Field: "name", Op: "contains", Value: "a", Enabled: true},
	})
	if err == nil || len(rules) != 1 {
		t.Fatalf("expected one valid rule and an error, got %d, %v", len(rules), err)
	}
}

func TestSourceScope(t *testing.T) {
	rules, _ := Compile([]store.Rule{{ID: 1, Action: "include", Field: "any", Op: "contains", Value: "cnn", SourceID: 2, Enabled: true}})
	f1 := NewFields(1, "CNN", "", "")
	f2 := NewFields(2, "CNN", "", "")
	if Evaluate(rules, &f1).Include || !Evaluate(rules, &f2).Include {
		t.Error("source scoping broken")
	}
}
