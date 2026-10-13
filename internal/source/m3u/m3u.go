// Package m3u is a streaming M3U/M3U8 playlist parser. It never holds the
// whole playlist in memory and drops VOD/series entries while parsing.
package m3u

import (
	"bufio"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"
)

type Entry struct {
	Name    string
	TvgName string
	TvgID   string
	Logo    string
	Group   string
	URL     string
}

type Header struct {
	EPGURL string // url-tvg / x-tvg-url
}

type Stats struct {
	Live    int
	Skipped int // VOD / series
}

// Parse reads the playlist and calls fn for every live entry.
func Parse(r io.Reader, onHeader func(Header), fn func(Entry) error) (Stats, error) {
	var st Stats
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var cur *Entry
	first := true
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if first {
			line = strings.TrimPrefix(line, "\ufeff")
			first = false
		}
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "#EXTM3U"):
			if onHeader != nil {
				attrs, _ := parseAttrs(strings.TrimPrefix(line, "#EXTM3U"))
				h := Header{EPGURL: attrs["url-tvg"]}
				if h.EPGURL == "" {
					h.EPGURL = attrs["x-tvg-url"]
				}
				// Some playlists list several comma-separated guides; take the first.
				if i := strings.IndexByte(h.EPGURL, ','); i > 0 {
					h.EPGURL = h.EPGURL[:i]
				}
				onHeader(h)
			}
		case strings.HasPrefix(line, "#EXTINF:"):
			e := parseExtinf(line[len("#EXTINF:"):])
			cur = &e
		case strings.HasPrefix(line, "#EXTGRP:"):
			if cur != nil && cur.Group == "" {
				cur.Group = strings.TrimSpace(line[len("#EXTGRP:"):])
			}
		case strings.HasPrefix(line, "#"):
			// #EXTVLCOPT etc. — ignored
		default:
			if cur == nil {
				continue
			}
			cur.URL = line
			if cur.Name == "" {
				cur.Name = cur.TvgName
			}
			if IsVOD(cur.URL) {
				st.Skipped++
			} else {
				st.Live++
				if err := fn(*cur); err != nil {
					return st, err
				}
			}
			cur = nil
		}
	}
	return st, sc.Err()
}

// parseExtinf parses `-1 tvg-id="a" group-title="b",Display Name`.
func parseExtinf(s string) Entry {
	// skip duration
	i := 0
	for i < len(s) && s[i] != ' ' && s[i] != ',' {
		i++
	}
	attrs, rest := parseAttrs(s[i:])
	return Entry{
		Name:    strings.TrimSpace(rest),
		TvgName: attrs["tvg-name"],
		TvgID:   attrs["tvg-id"],
		Logo:    attrs["tvg-logo"],
		Group:   attrs["group-title"],
	}
}

// parseAttrs reads key="value" pairs until an unquoted comma, returning the
// attributes and whatever follows the comma.
func parseAttrs(s string) (map[string]string, string) {
	attrs := map[string]string{}
	i := 0
	for i < len(s) {
		for i < len(s) && s[i] == ' ' {
			i++
		}
		if i >= len(s) {
			break
		}
		if s[i] == ',' {
			return attrs, s[i+1:]
		}
		ks := i
		for i < len(s) && s[i] != '=' && s[i] != ' ' && s[i] != ',' {
			i++
		}
		key := strings.ToLower(s[ks:i])
		if i >= len(s) || s[i] != '=' {
			continue
		}
		i++ // '='
		var val string
		if i < len(s) && (s[i] == '"' || s[i] == '\'') {
			q := s[i]
			i++
			vs := i
			for i < len(s) && s[i] != q {
				i++
			}
			val = s[vs:i]
			if i < len(s) {
				i++
			}
		} else {
			vs := i
			for i < len(s) && s[i] != ' ' && s[i] != ',' {
				i++
			}
			val = s[vs:i]
		}
		attrs[key] = strings.TrimSpace(val)
	}
	return attrs, ""
}

var vodExt = map[string]bool{".mp4": true, ".mkv": true, ".avi": true, ".m4v": true, ".mov": true, ".wmv": true, ".flv": true}

// IsVOD reports whether a stream URL points at a movie or series episode.
func IsVOD(raw string) bool {
	p := raw
	if u, err := url.Parse(raw); err == nil {
		p = u.Path
	}
	lp := strings.ToLower(p)
	if strings.Contains(lp, "/movie/") || strings.Contains(lp, "/series/") {
		return true
	}
	return vodExt[path.Ext(lp)]
}

var xtreamPath = regexp.MustCompile(`^/(?:live/)?[^/]+/[^/]+/(\d+)(?:\.[a-z0-9]+)?$`)

// StableKey derives an identifier that survives playlist refreshes. Xtream
// style URLs carry a stream id; otherwise group+name is used, with an
// occurrence counter to separate duplicates.
func StableKey(e Entry, dup map[string]int) string {
	if u, err := url.Parse(e.URL); err == nil {
		if m := xtreamPath.FindStringSubmatch(strings.ToLower(u.Path)); m != nil {
			return "sid:" + m[1]
		}
	}
	k := "n:" + e.Group + "|" + e.Name
	dup[k]++
	if n := dup[k]; n > 1 {
		return k + "#" + itoa(n)
	}
	return k
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
