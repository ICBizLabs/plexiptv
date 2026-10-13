// Package web serves the management panel and its JSON API.
package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"plexiptv/internal/app"
	"plexiptv/internal/logbuf"
)

//go:embed ui
var uiFS embed.FS

const cookieName = "plexiptv_session"

type Server struct {
	a    *app.App
	logs *logbuf.Buffer
	mux  *http.ServeMux

	// Cached auth settings so API requests never wait on the database
	// (e.g. while a large import transaction is committing).
	authMu    sync.RWMutex
	adminHash string
	secret    string
}

func New(a *app.App, logs *logbuf.Buffer) *Server {
	s := &Server{a: a, logs: logs, mux: http.NewServeMux()}
	ctx := context.Background()
	s.adminHash, _ = a.St.GetSetting(ctx, "admin_hash")
	s.secret, _ = a.St.GetSetting(ctx, "session_secret")
	if s.secret == "" {
		b := make([]byte, 32)
		rand.Read(b)
		s.secret = hex.EncodeToString(b)
		a.St.SetSetting(ctx, "session_secret", s.secret)
	}
	ui, _ := fs.Sub(uiFS, "ui")
	s.mux.Handle("GET /", http.FileServer(http.FS(ui)))

	s.mux.HandleFunc("GET /api/state", s.state)
	s.mux.HandleFunc("POST /api/setup", s.setup)
	s.mux.HandleFunc("POST /api/login", s.login)
	s.mux.HandleFunc("POST /api/logout", s.logout)

	s.route("GET /api/status", s.status)
	s.route("GET /api/logs", s.getLogs)

	s.route("GET /api/sources", s.listSources)
	s.route("POST /api/sources", s.saveSource)
	s.route("PUT /api/sources/{id}", s.saveSource)
	s.route("DELETE /api/sources/{id}", s.deleteSource)
	s.route("POST /api/sources/{id}/refresh", s.refreshSource)

	s.route("GET /api/channels", s.listChannels)
	s.route("PATCH /api/channels", s.patchChannels)
	s.route("POST /api/channels/renumber", s.renumber)
	s.route("GET /api/groups", s.listGroups)
	s.route("POST /api/groups/toggle", s.toggleGroup)

	s.route("GET /api/rules", s.listRules)
	s.route("POST /api/rules", s.saveRule)
	s.route("PUT /api/rules/{id}", s.saveRule)
	s.route("DELETE /api/rules/{id}", s.deleteRule)
	s.route("POST /api/rules/order", s.orderRules)
	s.route("POST /api/rules/preview", s.previewRule)

	s.route("GET /api/devices", s.listDevices)
	s.route("POST /api/devices", s.saveDevice)
	s.route("PUT /api/devices/{id}", s.saveDevice)
	s.route("DELETE /api/devices/{id}", s.deleteDevice)

	s.route("GET /api/epg/search", s.epgSearch)
	s.route("POST /api/epg/automatch", s.epgAutomatch)

	s.route("GET /api/streams", s.listStreams)
	s.route("DELETE /api/streams/{id}", s.stopStream)

	s.route("GET /api/settings", s.getSettings)
	s.route("PUT /api/settings", s.putSettings)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// route registers an authenticated API handler.
func (s *Server) route(pattern string, h http.HandlerFunc) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if !s.authed(r) {
			writeErr(w, http.StatusUnauthorized, errors.New("login required"))
			return
		}
		h(w, r)
	})
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8<<20))
	return dec.Decode(v)
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

func qInt(r *http.Request, k string) int64 {
	v, _ := strconv.ParseInt(r.URL.Query().Get(k), 10, 64)
	return v
}

// ---- auth ----

func (s *Server) hash(context.Context) string {
	s.authMu.RLock()
	defer s.authMu.RUnlock()
	return s.adminHash
}

func (s *Server) setHash(ctx context.Context, h string) error {
	if err := s.a.St.SetSetting(ctx, "admin_hash", h); err != nil {
		return err
	}
	s.authMu.Lock()
	s.adminHash = h
	s.authMu.Unlock()
	return nil
}

// Sessions are stateless signed cookies ("expiry.mac"), so they survive
// restarts. The MAC covers the password hash: changing the password logs
// every other browser out.
const sessionTTL = 30 * 24 * time.Hour

func (s *Server) sessionMAC(ctx context.Context, expiry string) []byte {
	s.authMu.RLock()
	secret := s.secret
	s.authMu.RUnlock()
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(expiry + "|" + s.hash(ctx)))
	return m.Sum(nil)
}

func (s *Server) authed(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	exp, mac, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	unix, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || time.Now().Unix() > unix {
		return false
	}
	got, err := hex.DecodeString(mac)
	return err == nil && s.hash(r.Context()) != "" && hmac.Equal(got, s.sessionMAC(r.Context(), exp))
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request) {
	exp := strconv.FormatInt(time.Now().Add(sessionTTL).Unix(), 10)
	tok := exp + "." + hex.EncodeToString(s.sessionMAC(r.Context(), exp))
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds())})
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]bool{"setup": s.hash(r.Context()) == "", "authed": s.authed(r)})
}

type pwBody struct {
	Password string `json:"password"`
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	if s.hash(r.Context()) != "" {
		writeErr(w, http.StatusForbidden, errors.New("already set up"))
		return
	}
	var b pwBody
	if err := readJSON(r, &b); err != nil || len(b.Password) < 4 {
		writeErr(w, http.StatusBadRequest, errors.New("password must be at least 4 characters"))
		return
	}
	h, err := bcryptHash(b.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.setHash(r.Context(), h); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.startSession(w, r)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var b pwBody
	readJSON(r, &b)
	h := s.hash(r.Context())
	if h == "" || bcrypt.CompareHashAndPassword([]byte(h), []byte(b.Password)) != nil {
		time.Sleep(500 * time.Millisecond)
		writeErr(w, http.StatusUnauthorized, errors.New("wrong password"))
		return
	}
	s.startSession(w, r)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, map[string]bool{"ok": true})
}

func bcryptHash(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(h), err
}
