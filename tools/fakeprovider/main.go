// fakeprovider is a development stand-in for an IPTV provider. It serves an
// Xtream Codes API, an M3U playlist (live + VOD + series), an XMLTV guide and
// endless MPEG-TS streams, so plexiptv can be load-tested without a real account.
//
//	go run ./tools/fakeprovider -live 60000 -vod 30000 -series 10000
//	Xtream: server http://localhost:8090  user: u  pass: p
//	M3U:    http://localhost:8090/get.php?username=u&password=p&type=m3u_plus
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

var (
	live    = flag.Int("live", 60000, "live channels")
	vod     = flag.Int("vod", 30000, "movies (M3U only)")
	series  = flag.Int("series", 10000, "series episodes (M3U only)")
	epgN    = flag.Int("epg", 5000, "channels with guide data")
	maxConn = flag.Int("max", 2, "max_connections reported")
	addr    = flag.String("addr", ":8090", "listen address")
	slow    = flag.Duration("slow", 0, "delay per 1000 streams/lines, to simulate a slow provider")
	progs   = flag.Int("progs", 12, "programmes (2h each) per guide channel")
	hlsDir  = flag.String("hls", "", "directory served at /hls/ (for HLS tests)")
	open    atomic.Int32
)

var countries = []string{"US", "UK", "CA", "DE", "FR", "ES", "IT", "NL", "PT", "AR", "MX", "IN", "AU", "SE", "PL"}
var genres = []string{"News", "Sports", "Movies", "Kids", "Documentary", "Music", "Entertainment", "Local", "PPV", "24/7", "Religious", "Lifestyle", "Business", "Comedy", "Drama", "Sci-Fi", "Horror", "Reality", "Weather", "Shopping", "Latino", "Asian", "Arabic", "4K", "Backup", "Events", "Adult", "Radio"}

func category(i int) (int, string) {
	c := i % (len(countries) * len(genres))
	return c + 1, countries[c/len(genres)] + " | " + genres[c%len(genres)]
}

func name(i int) string {
	_, cat := category(i)
	cc := cat[:2]
	q := []string{"HD", "FHD", "SD", "4K", "HEVC"}[i%5]
	return fmt.Sprintf("%s: Channel %d %s", cc, i, q)
}

func main() {
	flag.Parse()
	http.HandleFunc("/player_api.php", playerAPI)
	http.HandleFunc("/get.php", m3u)
	http.HandleFunc("/xmltv.php", xmltv)
	http.HandleFunc("/live/", stream)
	if *hlsDir != "" {
		http.Handle("/hls/", http.StripPrefix("/hls/", http.FileServer(http.Dir(*hlsDir))))
	}
	log.Printf("fake provider on %s: %d live, %d vod, %d series", *addr, *live, *vod, *series)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func playerAPI(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("username") != "u" || q.Get("password") != "p" {
		json.NewEncoder(w).Encode(map[string]any{"user_info": map[string]any{"auth": 0}})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	bw := bufio.NewWriterSize(w, 1<<16)
	defer bw.Flush()
	switch q.Get("action") {
	case "":
		json.NewEncoder(bw).Encode(map[string]any{"user_info": map[string]any{
			"auth": 1, "status": "Active", "max_connections": fmt.Sprint(*maxConn), "active_cons": fmt.Sprint(open.Load()),
		}})
	case "get_live_categories":
		var cats []map[string]string
		for c := 0; c < len(countries)*len(genres); c++ {
			id, n := category(c)
			cats = append(cats, map[string]string{"category_id": fmt.Sprint(id), "category_name": n})
		}
		json.NewEncoder(bw).Encode(cats)
	case "get_live_streams":
		bw.WriteString("[")
		enc := json.NewEncoder(bw)
		for i := 1; i <= *live; i++ {
			throttle(i, bw)
			if i > 1 {
				bw.WriteString(",")
			}
			cid, _ := category(i)
			epg := ""
			if i <= *epgN {
				epg = fmt.Sprintf("ch%d.fake", i)
			}
			enc.Encode(map[string]any{"num": i, "name": name(i), "stream_type": "live", "stream_id": i,
				"stream_icon": "", "epg_channel_id": epg, "category_id": fmt.Sprint(cid)})
		}
		bw.WriteString("]")
	default:
		bw.WriteString("[]")
	}
}

func throttle(i int, bw *bufio.Writer) {
	if *slow > 0 && i%1000 == 0 {
		bw.Flush()
		time.Sleep(*slow)
	}
}

func m3u(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "audio/x-mpegurl")
	bw := bufio.NewWriterSize(w, 1<<16)
	defer bw.Flush()
	host := "http://" + r.Host
	fmt.Fprintf(bw, "#EXTM3U url-tvg=\"%s/xmltv.php?username=u&password=p\"\n", host)
	for i := 1; i <= *live; i++ {
		throttle(i, bw)
		_, cat := category(i)
		fmt.Fprintf(bw, "#EXTINF:-1 tvg-id=\"ch%d.fake\" tvg-name=\"%s\" tvg-logo=\"\" group-title=\"%s\",%s\n%s/live/u/p/%d.ts\n",
			i, name(i), cat, name(i), host, i)
	}
	for i := 1; i <= *vod; i++ {
		fmt.Fprintf(bw, "#EXTINF:-1 group-title=\"VOD | Movies\",Movie %d (2020)\n%s/movie/u/p/%d.mkv\n", i, host, 900000+i)
	}
	for i := 1; i <= *series; i++ {
		fmt.Fprintf(bw, "#EXTINF:-1 group-title=\"Series\",Show %d S01E01\n%s/series/u/p/%d.mp4\n", i, host, 800000+i)
	}
}

func xmltv(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/xml")
	bw := bufio.NewWriterSize(w, 1<<16)
	defer bw.Flush()
	bw.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n<tv>\n")
	for i := 1; i <= *epgN; i++ {
		fmt.Fprintf(bw, "<channel id=\"ch%d.fake\"><display-name>Channel %d</display-name></channel>\n", i, i)
	}
	start := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	for i := 1; i <= *epgN; i++ {
		for p := 0; p < *progs; p++ {
			s := start.Add(time.Duration(p) * 2 * time.Hour)
			fmt.Fprintf(bw, "<programme start=\"%s\" stop=\"%s\" channel=\"ch%d.fake\"><title>Show %d &amp; friends</title><desc>Episode %d</desc></programme>\n",
				s.Format("20060102150405 -0700"), s.Add(2*time.Hour).Format("20060102150405 -0700"), i, p, p)
		}
	}
	bw.WriteString("</tv>\n")
}

func stream(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 4 || parts[1] != "u" || parts[2] != "p" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if int(open.Load()) >= *maxConn {
		http.Error(w, "max connections reached", http.StatusForbidden)
		return
	}
	open.Add(1)
	defer open.Add(-1)
	log.Printf("stream open %s (open=%d)", parts[3], open.Load())
	w.Header().Set("Content-Type", "video/mp2t")
	pkt := make([]byte, 188*350)
	for i := 0; i < len(pkt); i += 188 {
		pkt[i] = 0x47
	}
	for {
		if _, err := w.Write(pkt); err != nil {
			log.Printf("stream closed %s", parts[3])
			return
		}
		w.(http.Flusher).Flush()
		time.Sleep(50 * time.Millisecond)
	}
}
