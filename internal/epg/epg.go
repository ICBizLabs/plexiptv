// Package epg downloads XMLTV guides and produces a filtered XMLTV document
// containing only mapped channels, using streaming XML parsing throughout.
package epg

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"plexiptv/internal/ident"
)

// Download fetches url to dest, transparently gunzipping when needed.
// Local file paths are also accepted.
func Download(ctx context.Context, hc *http.Client, url, ua, dest string) error {
	var body io.ReadCloser
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		ident.Apply(req.Header, ua, false)
		resp, err := hc.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return fmt.Errorf("EPG download returned HTTP %d", resp.StatusCode)
		}
		body = resp.Body
	} else {
		f, err := os.Open(url)
		if err != nil {
			return err
		}
		body = f
	}
	defer body.Close()

	br := bufio.NewReaderSize(body, 256*1024)
	var r io.Reader = br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	}
	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return renameRetry(tmp, dest)
}

// renameRetry tolerates the destination being briefly open (on Windows a
// file being read by the guide builder can't be replaced).
func renameRetry(from, to string) error {
	var err error
	for i := 0; i < 20; i++ {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	os.Remove(from)
	return err
}

type Channel struct {
	ID   string
	Name string
	Icon string
}

func newDecoder(r io.Reader) *xml.Decoder {
	d := xml.NewDecoder(bufio.NewReaderSize(r, 256*1024))
	d.Strict = false
	d.Entity = xml.HTMLEntity
	d.CharsetReader = charsetReader
	return d
}

// ScanChannels lists the <channel> elements of an XMLTV file, skipping programmes.
func ScanChannels(path string) ([]Channel, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	d := newDecoder(f)
	var out []Channel
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "tv":
		case "channel":
			var c struct {
				ID    string   `xml:"id,attr"`
				Names []string `xml:"display-name"`
				Icon  struct {
					Src string `xml:"src,attr"`
				} `xml:"icon"`
			}
			if err := d.DecodeElement(&c, &se); err != nil {
				return out, err
			}
			ch := Channel{ID: strings.TrimSpace(c.ID), Icon: c.Icon.Src}
			if len(c.Names) > 0 {
				ch.Name = strings.TrimSpace(c.Names[0])
			}
			if ch.ID != "" {
				out = append(out, ch)
			}
		default:
			if err := d.Skip(); err != nil {
				return out, err
			}
		}
	}
}

// OutChannel is a channel as it appears in the generated guide; ID is the
// lineup GuideNumber so Plex lines guide and tuner up automatically.
type OutChannel struct {
	ID   string
	Name string
	Logo string
}

type File struct {
	Path string
	// Owns reports whether this file is the chosen provider for an EPG id
	// (an id present in several files is taken from the first only).
	Owns func(epgID string) bool
}

type Plan struct {
	Channels []OutChannel        // every channel in the lineup
	Map      map[string][]string // source EPG id -> output channel ids
	Dummy    []OutChannel        // channels without guide data
	Files    []File
	Now      time.Time
}

// Generate writes the XMLTV document.
func Generate(w io.Writer, p Plan) error {
	bw := bufio.NewWriterSize(w, 256*1024)
	bw.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<tv generator-info-name="plexiptv">` + "\n")
	for _, c := range p.Channels {
		bw.WriteString(`<channel id="`)
		escAttr(bw, c.ID)
		bw.WriteString(`"><display-name>`)
		xml.EscapeText(bw, []byte(c.Name))
		bw.WriteString(`</display-name>`)
		if c.Logo != "" {
			bw.WriteString(`<icon src="`)
			escAttr(bw, c.Logo)
			bw.WriteString(`"/>`)
		}
		bw.WriteString("</channel>\n")
	}
	cutoff := p.Now.Add(-6 * time.Hour)
	for _, f := range p.Files {
		if err := copyProgrammes(bw, f, p.Map, cutoff); err != nil {
			return fmt.Errorf("%s: %w", f.Path, err)
		}
	}
	writeDummy(bw, p.Dummy, p.Now)
	bw.WriteString("</tv>\n")
	return bw.Flush()
}

type rawElem struct {
	Attrs []xml.Attr `xml:",any,attr"`
	Inner []byte     `xml:",innerxml"`
}

func copyProgrammes(w *bufio.Writer, f File, want map[string][]string, cutoff time.Time) error {
	fh, err := os.Open(f.Path)
	if err != nil {
		return err
	}
	defer fh.Close()
	d := newDecoder(fh)
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "tv":
			continue
		case "programme":
		default:
			if err := d.Skip(); err != nil {
				return err
			}
			continue
		}
		var ch, stop string
		for _, a := range se.Attr {
			switch a.Name.Local {
			case "channel":
				ch = a.Value
			case "stop":
				stop = a.Value
			}
		}
		outs := want[ch]
		if len(outs) == 0 || !f.Owns(ch) {
			if err := d.Skip(); err != nil {
				return err
			}
			continue
		}
		if t, ok := parseTime(stop); ok && t.Before(cutoff) {
			if err := d.Skip(); err != nil {
				return err
			}
			continue
		}
		var el rawElem
		if err := d.DecodeElement(&el, &se); err != nil {
			return err
		}
		for _, out := range outs {
			w.WriteString("<programme")
			for _, a := range se.Attr {
				v := a.Value
				if a.Name.Local == "channel" {
					v = out
				}
				w.WriteString(" " + a.Name.Local + `="`)
				escAttr(w, v)
				w.WriteString(`"`)
			}
			w.WriteString(">")
			w.Write(el.Inner)
			w.WriteString("</programme>\n")
		}
	}
}

const xmltvTime = "20060102150405 -0700"

func parseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(xmltvTime, s); err == nil {
		return t, true
	}
	if len(s) >= 14 {
		if t, err := time.Parse("20060102150405", s[:14]); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// writeDummy emits 2-hour blocks for 3 days so Plex keeps channels that have
// no real guide data (it hides channels with an empty guide).
func writeDummy(w *bufio.Writer, chans []OutChannel, now time.Time) {
	start := now.UTC().Truncate(2 * time.Hour).Add(-2 * time.Hour)
	for _, c := range chans {
		for i := 0; i < 37; i++ {
			s := start.Add(time.Duration(i) * 2 * time.Hour)
			e := s.Add(2 * time.Hour)
			w.WriteString(`<programme start="` + s.Format(xmltvTime) + `" stop="` + e.Format(xmltvTime) + `" channel="`)
			escAttr(w, c.ID)
			w.WriteString(`"><title lang="en">`)
			xml.EscapeText(w, []byte(c.Name))
			w.WriteString("</title></programme>\n")
		}
	}
}

func escAttr(w io.Writer, s string) {
	xml.EscapeText(w, []byte(s)) // escapes & < > " ' and control whitespace
}

// charsetReader supports the common non-UTF-8 XMLTV encoding (ISO-8859-1).
func charsetReader(label string, in io.Reader) (io.Reader, error) {
	switch strings.ToLower(label) {
	case "utf-8", "utf8", "us-ascii", "ascii":
		return in, nil
	case "iso-8859-1", "latin1", "latin-1", "windows-1252", "cp1252":
		return &latin1Reader{r: bufio.NewReader(in)}, nil
	}
	return nil, fmt.Errorf("unsupported charset %q", label)
}

type latin1Reader struct {
	r   *bufio.Reader
	buf []byte
}

func (l *latin1Reader) Read(p []byte) (int, error) {
	for len(l.buf) < len(p) {
		b, err := l.r.ReadByte()
		if err != nil {
			if len(l.buf) > 0 {
				break
			}
			return 0, err
		}
		l.buf = utf8.AppendRune(l.buf, rune(b))
	}
	n := copy(p, l.buf)
	l.buf = l.buf[n:]
	return n, nil
}
