// Package xtream is a minimal Xtream Codes player_api client. It only asks
// for live categories and live streams, so VOD and series are never downloaded.
package xtream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"plexiptv/internal/ident"
)

type Client struct {
	Base string // scheme://host[:port]
	User string
	Pass string
	UA   string
	HTTP *http.Client
}

// New normalises the server URL a user typed (with or without scheme,
// trailing slash or /player_api.php).
func New(server, user, pass, ua string, hc *http.Client) *Client {
	s := strings.TrimSpace(server)
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	if u, err := url.Parse(s); err == nil {
		s = u.Scheme + "://" + u.Host
	}
	return &Client{Base: s, User: user, Pass: pass, UA: ua, HTTP: hc}
}

// FlexString accepts JSON strings, numbers and null.
type FlexString string

func (f *FlexString) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*f = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = FlexString(s)
		return nil
	}
	*f = FlexString(b)
	return nil
}

type UserInfo struct {
	Auth           int        `json:"auth"`
	Status         FlexString `json:"status"`
	ExpDate        FlexString `json:"exp_date"`
	MaxConnections FlexString `json:"max_connections"`
	ActiveCons     FlexString `json:"active_cons"`
	// AllowedOutputFormats is e.g. ["m3u8","ts","rtmp"]; some panels send it
	// as something other than a list, which is tolerated.
	AllowedOutputFormats FlexList `json:"allowed_output_formats"`
}

// FlexList accepts a JSON list of strings/numbers and ignores anything else.
type FlexList []FlexString

func (f *FlexList) UnmarshalJSON(b []byte) error {
	var list []FlexString
	if json.Unmarshal(b, &list) == nil {
		*f = list
	}
	return nil
}

func (u UserInfo) MaxConns() int {
	n, _ := strconv.Atoi(string(u.MaxConnections))
	return n
}

type Stream struct {
	Num          FlexString `json:"num"`
	Name         string     `json:"name"`
	StreamType   FlexString `json:"stream_type"`
	StreamID     FlexString `json:"stream_id"`
	StreamIcon   string     `json:"stream_icon"`
	EPGChannelID FlexString `json:"epg_channel_id"`
	CategoryID   FlexString `json:"category_id"`
}

func (c *Client) api(action string) string {
	q := url.Values{"username": {c.User}, "password": {c.Pass}}
	if action != "" {
		q.Set("action", action)
	}
	return c.Base + "/player_api.php?" + q.Encode()
}

func (c *Client) get(ctx context.Context, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	ident.Apply(req.Header, c.UA, false)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, redactErr(err, c)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("provider returned HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

func (c *Client) Info(ctx context.Context) (UserInfo, error) {
	resp, err := c.get(ctx, c.api(""))
	if err != nil {
		return UserInfo{}, err
	}
	defer resp.Body.Close()
	var body struct {
		UserInfo UserInfo `json:"user_info"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return UserInfo{}, fmt.Errorf("decode user_info: %w", err)
	}
	if body.UserInfo.Auth != 1 {
		return body.UserInfo, errors.New("xtream login failed (check server, username and password)")
	}
	return body.UserInfo, nil
}

func (c *Client) LiveCategories(ctx context.Context) (map[string]string, error) {
	resp, err := c.get(ctx, c.api("get_live_categories"))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var list []struct {
		ID   FlexString `json:"category_id"`
		Name string     `json:"category_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decode categories: %w", err)
	}
	out := make(map[string]string, len(list))
	for _, c := range list {
		out[string(c.ID)] = c.Name
	}
	return out, nil
}

// DownloadLiveStreams copies the raw (often very large) get_live_streams
// response to w. Callers spool it to disk so the database transaction that
// imports it isn't held open for the whole download.
func (c *Client) DownloadLiveStreams(ctx context.Context, w io.Writer) error {
	resp, err := c.get(ctx, c.api("get_live_streams"))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(w, resp.Body); err != nil {
		return redactErr(err, c)
	}
	return nil
}

// DecodeLiveStreams decodes a get_live_streams response one element at a time.
func DecodeLiveStreams(r io.Reader, fn func(Stream) error) error {
	dec := json.NewDecoder(r)
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("decode streams: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return errors.New("unexpected get_live_streams response (not a list)")
	}
	for dec.More() {
		var s Stream
		if err := dec.Decode(&s); err != nil {
			return fmt.Errorf("decode stream: %w", err)
		}
		if err := fn(s); err != nil {
			return err
		}
	}
	return nil
}

// StreamExt picks the container to request: "ts" unless the account only
// allows HLS.
func (u UserInfo) StreamExt() string {
	if len(u.AllowedOutputFormats) == 0 {
		return "ts"
	}
	hasM3U8 := false
	for _, f := range u.AllowedOutputFormats {
		switch strings.ToLower(string(f)) {
		case "ts":
			return "ts"
		case "m3u8":
			hasM3U8 = true
		}
	}
	if hasM3U8 {
		return "m3u8"
	}
	return "ts"
}

func (c *Client) StreamURL(streamID, ext string) string {
	if ext == "" {
		ext = "ts"
	}
	return c.Base + "/live/" + url.PathEscape(c.User) + "/" + url.PathEscape(c.Pass) + "/" + streamID + "." + ext
}

func (c *Client) EPGURL() string {
	q := url.Values{"username": {c.User}, "password": {c.Pass}}
	return c.Base + "/xmltv.php?" + q.Encode()
}

func redactErr(err error, c *Client) error {
	msg := err.Error()
	for _, s := range []string{c.Pass, url.QueryEscape(c.Pass), c.User, url.QueryEscape(c.User)} {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, "***")
		}
	}
	return errors.New(msg)
}
