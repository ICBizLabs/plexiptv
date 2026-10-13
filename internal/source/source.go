// Package source refreshes playlists and guides from Xtream / M3U / XMLTV sources.
package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"plexiptv/internal/epg"
	"plexiptv/internal/redact"
	"plexiptv/internal/source/m3u"
	"plexiptv/internal/source/xtream"
	"plexiptv/internal/store"
)

type Refresher struct {
	St      *store.Store
	HTTP    *http.Client
	DataDir string
	Log     *slog.Logger
}

func (r *Refresher) EPGPath(sourceID int64) string {
	return filepath.Join(r.DataDir, "epg", fmt.Sprintf("%d.xml", sourceID))
}

// Result summarises a refresh for the UI.
type Result struct {
	Channels int           `json:"channels"`
	Skipped  int           `json:"skipped"`
	EPG      int           `json:"epg_channels"`
	Took     time.Duration `json:"took_ms"`
}

// Refresh imports channels (unless the source is EPG-only) and then its guide.
// An EPG failure is recorded but does not fail the channel import.
func (r *Refresher) Refresh(ctx context.Context, src store.Source) (Result, error) {
	start := time.Now()
	var res Result
	var epgURL string
	var err error
	switch src.Kind {
	case "xtream":
		epgURL, err = r.importXtream(ctx, src, &res)
	case "m3u":
		epgURL, err = r.importM3U(ctx, src, &res)
	case "epg":
		epgURL = src.URL
	default:
		err = fmt.Errorf("unknown source kind %q", src.Kind)
	}
	now := time.Now().Unix()
	if err != nil {
		msg := redact.Secrets(err.Error(), src.Username, src.Password)
		r.St.SetSourceError(context.Background(), src.ID, now, msg)
		return res, errors.New(msg)
	}
	if src.EPGURL != "" {
		epgURL = src.EPGURL
	}
	if epgURL != "" {
		n, eerr := r.refreshEPG(ctx, src, epgURL)
		res.EPG = n
		msg := ""
		if eerr != nil {
			msg = redact.Secrets(eerr.Error(), src.Username, src.Password)
			r.Log.Warn("epg refresh failed", "source", src.Name, "err", msg)
		}
		r.St.SetEPGStatus(context.Background(), src.ID, n, msg)
	}
	if src.Kind == "epg" {
		r.St.SetSourceStatus(context.Background(), src.ID, now, "", 0, 0, 0)
	}
	res.Took = time.Since(start) / time.Millisecond
	r.Log.Info("source refreshed", "source", src.Name, "channels", res.Channels, "skipped_vod", res.Skipped,
		"epg_channels", res.EPG, "took", time.Since(start).Round(time.Millisecond))
	return res, nil
}

func (r *Refresher) importXtream(ctx context.Context, src store.Source, res *Result) (string, error) {
	cl := xtream.New(src.URL, src.Username, src.Password, src.UserAgent, r.HTTP)
	info, err := cl.Info(ctx)
	if err != nil {
		return "", err
	}
	cats, err := cl.LiveCategories(ctx)
	if err != nil {
		return "", err
	}
	spool, err := r.spool(src.ID, func(w io.Writer) error { return cl.DownloadLiveStreams(ctx, w) })
	if err != nil {
		return "", err
	}
	defer spool.Close()
	im, err := r.St.BeginImport(ctx, src.ID, time.Now().UnixNano())
	if err != nil {
		return "", err
	}
	err = xtream.DecodeLiveStreams(spool, func(s xtream.Stream) error {
		if s.StreamID == "" {
			return nil
		}
		if t := string(s.StreamType); t != "" && t != "live" && t != "created_live" && t != "radio_streams" {
			res.Skipped++
			return nil
		}
		return im.Add(ctx, store.Channel{
			ExtKey: "sid:" + string(s.StreamID),
			Name:   strings.TrimSpace(s.Name),
			Group:  cats[string(s.CategoryID)],
			Logo:   s.StreamIcon,
			TvgID:  string(s.EPGChannelID),
		})
	})
	if err == nil && im.Count == 0 {
		err = errors.New("provider returned no live channels; keeping previous list")
	}
	if err != nil {
		im.Rollback()
		return "", err
	}
	if err := im.Commit(ctx); err != nil {
		return "", err
	}
	res.Channels = im.Count
	r.St.SetSourceStatus(context.Background(), src.ID, time.Now().Unix(), "", im.Count, res.Skipped, info.MaxConns())
	if ext := info.StreamExt(); ext != src.StreamExt {
		r.Log.Info("provider stream format", "source", src.Name, "format", ext)
		r.St.SetStreamExt(context.Background(), src.ID, ext)
	}
	return cl.EPGURL(), nil
}

func (r *Refresher) importM3U(ctx context.Context, src store.Source, res *Result) (string, error) {
	var body io.ReadCloser
	if isURL(src.URL) {
		f, err := r.spool(src.ID, func(w io.Writer) error {
			rc, err := r.open(ctx, src.URL, src.UserAgent)
			if err != nil {
				return err
			}
			defer rc.Close()
			_, err = io.Copy(w, rc)
			return err
		})
		if err != nil {
			return "", err
		}
		body = f
	} else {
		f, err := os.Open(src.URL)
		if err != nil {
			return "", err
		}
		body = f
	}
	defer body.Close()
	im, err := r.St.BeginImport(ctx, src.ID, time.Now().UnixNano())
	if err != nil {
		return "", err
	}
	var hdrEPG string
	dup := map[string]int{}
	st, err := m3u.Parse(body, func(h m3u.Header) { hdrEPG = h.EPGURL }, func(e m3u.Entry) error {
		return im.Add(ctx, store.Channel{
			ExtKey: m3u.StableKey(e, dup),
			Name:   e.Name,
			Group:  e.Group,
			Logo:   e.Logo,
			TvgID:  e.TvgID,
			URL:    e.URL,
		})
	})
	if err == nil && im.Count == 0 {
		err = errors.New("playlist contained no live channels; keeping previous list")
	}
	if err != nil {
		im.Rollback()
		return "", err
	}
	if err := im.Commit(ctx); err != nil {
		return "", err
	}
	res.Channels, res.Skipped = st.Live, st.Skipped
	r.St.SetSourceStatus(context.Background(), src.ID, time.Now().Unix(), "", st.Live, st.Skipped, 0)
	return hdrEPG, nil
}

func isURL(u string) bool { return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") }

// spoolFile is a temp download that deletes itself on Close.
type spoolFile struct{ *os.File }

func (s spoolFile) Close() error {
	s.File.Close()
	return os.Remove(s.File.Name())
}

// spool downloads into a temp file and returns it rewound, so the import
// transaction only runs while parsing local data, not during a slow download.
func (r *Refresher) spool(sourceID int64, fetch func(io.Writer) error) (io.ReadCloser, error) {
	dir := filepath.Join(r.DataDir, "tmp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, fmt.Sprintf("import-%d-*", sourceID))
	if err != nil {
		return nil, err
	}
	sf := spoolFile{f}
	if err := fetch(f); err != nil {
		sf.Close()
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		sf.Close()
		return nil, err
	}
	return sf, nil
}

func (r *Refresher) open(ctx context.Context, u, ua string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("playlist download returned HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

func (r *Refresher) refreshEPG(ctx context.Context, src store.Source, u string) (int, error) {
	path := r.EPGPath(src.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, err
	}
	if err := epg.Download(ctx, r.HTTP, u, src.UserAgent, path); err != nil {
		return 0, err
	}
	chans, err := epg.ScanChannels(path)
	if err != nil && len(chans) == 0 {
		return 0, err
	}
	list := make([]store.EPGChannel, len(chans))
	for i, c := range chans {
		list[i] = store.EPGChannel{ID: c.ID, Name: c.Name, Icon: c.Icon}
	}
	if err := r.St.ReplaceEPGChannels(ctx, src.ID, list); err != nil {
		return 0, err
	}
	return len(list), nil
}

// StreamURL builds the upstream URL for a channel. Xtream URLs are built at
// play time so credentials are never stored per channel.
func StreamURL(src store.Source, extKey, url string) string {
	if src.Kind == "xtream" {
		cl := xtream.New(src.URL, src.Username, src.Password, src.UserAgent, nil)
		return cl.StreamURL(strings.TrimPrefix(extKey, "sid:"), src.StreamExt)
	}
	return url
}
