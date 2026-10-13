// Package redact strips credentials from URLs and messages before logging.
package redact

import (
	"net/url"
	"regexp"
	"strings"
)

var xtreamPath = regexp.MustCompile(`/(live|movie|series)/[^/]+/[^/]+/`)
var credQuery = regexp.MustCompile(`(?i)(username|password|user|pass|token)=[^&\s"]+`)

// URL hides Xtream path credentials and credential-like query parameters.
func URL(s string) string {
	s = xtreamPath.ReplaceAllString(s, "/$1/***/***/")
	s = credQuery.ReplaceAllString(s, "$1=***")
	if u, err := url.Parse(s); err == nil && u.User != nil {
		u.User = url.User("***")
		s = u.String()
	}
	return s
}

// Secrets replaces each non-empty secret (raw and URL-escaped) in msg.
func Secrets(msg string, secrets ...string) string {
	for _, sec := range secrets {
		if len(sec) < 2 {
			continue
		}
		msg = strings.ReplaceAll(msg, sec, "***")
		msg = strings.ReplaceAll(msg, url.QueryEscape(sec), "***")
		msg = strings.ReplaceAll(msg, url.PathEscape(sec), "***")
	}
	return URL(msg)
}
