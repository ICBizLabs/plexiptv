package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"plexiptv/internal/app"
	"plexiptv/internal/config"
	"plexiptv/internal/logbuf"
)

func newServer(t *testing.T, dir string) (*Server, *app.App) {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = dir
	a, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return New(a, logbuf.New(10)), a
}

func do(s *Server, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestAuthFlow(t *testing.T) {
	dir := t.TempDir()
	s, a := newServer(t, dir)

	if rec := do(s, "GET", "/api/sources", "", nil); rec.Code != 401 {
		t.Fatalf("unauthenticated API should be 401, got %d", rec.Code)
	}
	rec := do(s, "POST", "/api/setup", `{"password":"secret1"}`, nil)
	if rec.Code != 200 {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body)
	}
	cookie := rec.Result().Cookies()[0]
	if rec := do(s, "POST", "/api/setup", `{"password":"again"}`, nil); rec.Code != 403 {
		t.Fatalf("second setup must be refused, got %d", rec.Code)
	}
	if rec := do(s, "GET", "/api/sources", "", cookie); rec.Code != 200 {
		t.Fatalf("cookie rejected: %d", rec.Code)
	}

	// Credentials never come back from the API.
	do(s, "POST", "/api/sources", `{"kind":"xtream","url":"http://127.0.0.1:1","username":"u","password":"topsecret","refresh_hours":0}`, cookie)
	if body := do(s, "GET", "/api/sources", "", cookie).Body.String(); strings.Contains(body, "topsecret") || !strings.Contains(body, `"has_password":true`) {
		t.Fatalf("password leaked or flag missing: %s", body)
	}

	// Sessions survive a restart.
	a.St.Close()
	s2, a2 := newServer(t, dir)
	defer a2.St.Close()
	if rec := do(s2, "GET", "/api/sources", "", cookie); rec.Code != 200 {
		t.Fatalf("session lost across restart: %d", rec.Code)
	}

	// Changing the password invalidates other sessions but keeps this one.
	rec = do(s2, "PUT", "/api/settings", `{"new_password":"newpass"}`, cookie)
	fresh := rec.Result().Cookies()[0]
	if rec := do(s2, "GET", "/api/sources", "", cookie); rec.Code != 401 {
		t.Fatalf("old session still valid after password change: %d", rec.Code)
	}
	if rec := do(s2, "GET", "/api/sources", "", fresh); rec.Code != 200 {
		t.Fatalf("current session dropped: %d", rec.Code)
	}
	if rec := do(s2, "POST", "/api/login", `{"password":"secret1"}`, nil); rec.Code != 401 {
		t.Fatalf("old password accepted: %d", rec.Code)
	}
}
