// Package filter evaluates ordered include/exclude rules against channels.
//
// Rules are applied top to bottom like a .gitignore: every matching rule
// overrides the decision of the ones above it, so broad rules go first
// ("include group US | Sports") and exceptions after ("exclude name contains 4K").
package filter

import (
	"fmt"
	"regexp"
	"strings"

	"plexiptv/internal/store"
)

// Fields are the channel attributes rules can match. The L* fields must be
// lower-cased copies; the catalog precomputes them once per import.
type Fields struct {
	SourceID int64
	LName    string
	LGroup   string
	LTvgID   string
}

func NewFields(sourceID int64, name, group, tvgID string) Fields {
	return Fields{SourceID: sourceID, LName: strings.ToLower(name), LGroup: strings.ToLower(group), LTvgID: strings.ToLower(tvgID)}
}

type Rule struct {
	store.Rule
	lv string
	re *regexp.Regexp
}

// Compile prepares rules for matching. Disabled rules are dropped; rules with
// an invalid regex are returned as errors and skipped.
func Compile(rules []store.Rule) ([]Rule, error) {
	out := make([]Rule, 0, len(rules))
	var errs []string
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		c, err := CompileOne(r)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		out = append(out, c)
	}
	if len(errs) > 0 {
		return out, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return out, nil
}

func CompileOne(r store.Rule) (Rule, error) {
	c := Rule{Rule: r, lv: strings.ToLower(strings.TrimSpace(r.Value))}
	switch r.Op {
	case "equals", "contains", "prefix", "suffix":
	case "regex":
		re, err := regexp.Compile("(?i)" + r.Value)
		if err != nil {
			return c, fmt.Errorf("rule %d: bad regex: %v", r.ID, err)
		}
		c.re = re
	default:
		return c, fmt.Errorf("rule %d: unknown operator %q", r.ID, r.Op)
	}
	switch r.Field {
	case "group", "name", "tvg_id", "any":
	default:
		return c, fmt.Errorf("rule %d: unknown field %q", r.ID, r.Field)
	}
	if r.Action != "include" && r.Action != "exclude" {
		return c, fmt.Errorf("rule %d: unknown action %q", r.ID, r.Action)
	}
	return c, nil
}

func (r *Rule) matchStr(s string) bool {
	switch r.Op {
	case "equals":
		return s == r.lv
	case "contains":
		return strings.Contains(s, r.lv)
	case "prefix":
		return strings.HasPrefix(s, r.lv)
	case "suffix":
		return strings.HasSuffix(s, r.lv)
	case "regex":
		return r.re.MatchString(s)
	}
	return false
}

func (r *Rule) Match(f *Fields) bool {
	if r.SourceID != 0 && r.SourceID != f.SourceID {
		return false
	}
	switch r.Field {
	case "group":
		return r.matchStr(f.LGroup)
	case "name":
		return r.matchStr(f.LName)
	case "tvg_id":
		return r.matchStr(f.LTvgID)
	case "any":
		return r.matchStr(f.LName) || r.matchStr(f.LGroup) || r.matchStr(f.LTvgID)
	}
	return false
}

// Result of evaluating all rules for one channel.
type Result struct {
	Include  bool
	DeviceID int64 // 0 = default device
	RuleID   int64 // last rule that matched, 0 = none
}

func Evaluate(rules []Rule, f *Fields) Result {
	var res Result
	for i := range rules {
		r := &rules[i]
		if !r.Match(f) {
			continue
		}
		res.RuleID = r.ID
		res.Include = r.Action == "include"
		if res.Include && r.DeviceID != 0 {
			res.DeviceID = r.DeviceID
		}
	}
	return res
}
