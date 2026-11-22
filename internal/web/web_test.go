package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/neoautomata/brd/internal/auth"
	"github.com/neoautomata/brd/internal/config"
	"github.com/neoautomata/brd/internal/transit"
)

type stubCalendar struct {
	html   string
	err    error
	expiry string
}

func (s stubCalendar) RenderHTML(_ context.Context, _ time.Time) (string, error) {
	return s.html, s.err
}
func (s stubCalendar) TokenExpiry() string { return s.expiry }
func (s stubCalendar) RedirectForToken(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusSeeOther)
}
func (s stubCalendar) ProcessAuthCode(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusSeeOther)
}

type stubTransit struct {
	lines []transit.LineDepartures
}

func (s stubTransit) NextDepartures(_ context.Context, _ time.Time, _ *time.Location) []transit.LineDepartures {
	return s.lines
}

type stubRegen struct {
	calledDate string
	err        error
}

func (s *stubRegen) Regenerate(_ context.Context, dateStr string) error {
	s.calledDate = dateStr
	return s.err
}

func newTestServer(t *testing.T, regen *stubRegen) (*Server, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Parse([]byte(`{
		"data_dir": "`+dir+`",
		"timezone": "Europe/Zurich",
		"admin_users": ["admin@example.com"],
		"editor_users": ["editor@example.com"]
	}`), "")
	if err != nil {
		t.Fatalf("Parse config: %v", err)
	}
	for _, d := range []string{cfg.ContentsDir(), cfg.ScreenshotsDir(), cfg.ImagesDir()} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}

	zurich := cfg.Location
	fixedNow := time.Date(2026, 6, 15, 10, 0, 0, 0, zurich)
	authz := auth.NewAuthorizer(cfg.AdminUsers, cfg.EditorUsers, auth.HeaderResolver{})

	srv := NewServer(Dependencies{
		Config:      cfg,
		Authorizer:  authz,
		Calendar:    stubCalendar{html: "<ul><li>Meeting</li></ul>", expiry: "2026-06-15T11:00:00+02:00"},
		Transit:     stubTransit{lines: []transit.LineDepartures{{Line: "9", Next: "10:05"}, {Line: "14", Next: "10:10"}}},
		Regenerator: regen,
		Now:         func() time.Time { return fixedNow },
	})
	return srv, cfg
}

func extractCSRF(t *testing.T, body string) string {
	t.Helper()
	re := regexp.MustCompile(`name="csrf_token" value="([a-f0-9]+)"`)
	m := re.FindStringSubmatch(body)
	if len(m) != 2 {
		t.Fatalf("CSRF token not found in edit HTML")
	}
	return m[1]
}

func TestViewComputesServerLinksAndPreservesDimensions(t *testing.T) {
	srv, _ := newTestServer(t, &stubRegen{})
	h := srv.Handler()

	// Today: no Prev link, Edit and Next present
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.HasPrefix(body, "<!DOCTYPE html>") {
		t.Error("expected <!DOCTYPE html> prefix")
	}
	if strings.Contains(body, `id="prevdaylink"`) {
		t.Error("expected no Prev link on today")
	}
	if !strings.Contains(body, `href="/edit?date=2026-06-15"`) {
		t.Error("expected server-rendered Edit link for today")
	}
	if !strings.Contains(body, `href="/?date=2026-06-16"`) {
		t.Error("expected server-rendered Next link for tomorrow")
	}
	if !strings.Contains(body, "height: 960px") || !strings.Contains(body, "width: 540px") {
		t.Error("expected #screenshot_content 540x960 dimensions preserved")
	}
	for _, want := range []string{
		`<html lang="en">`,
		`<meta name="viewport" content="width=device-width, initial-scale=1">`,
		`<header `,
		`<nav `,
		`<main `,
		`<footer `,
		`:focus-visible`,
		`prefers-reduced-motion`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("GET / missing expected accessibility/layout marker %q", want)
		}
	}

	// Tomorrow: Prev link appears
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/?date=2026-06-16", nil))
	if rr2.Code != http.StatusOK {
		t.Fatalf("GET /?date=2026-06-16 status = %d, want 200", rr2.Code)
	}
	if !strings.Contains(rr2.Body.String(), `href="/?date=2026-06-15"`) {
		t.Error("expected Prev link pointing to today")
	}

	// Yesterday: rejected
	rr3 := httptest.NewRecorder()
	h.ServeHTTP(rr3, httptest.NewRequest(http.MethodGet, "/?date=2026-06-14", nil))
	if rr3.Code != http.StatusBadRequest {
		t.Errorf("prior day status = %d, want 400", rr3.Code)
	}
}

func TestEditorCanEditAndUpdateWithCSRFAndSanitization(t *testing.T) {
	regen := &stubRegen{}
	srv, cfg := newTestServer(t, regen)
	h := srv.Handler()

	// Editor opens /edit
	editReq := httptest.NewRequest(http.MethodGet, "/edit?date=2026-06-15", nil)
	editReq.Header.Set("X-Webauth-User", "editor@example.com")
	editRR := httptest.NewRecorder()
	h.ServeHTTP(editRR, editReq)
	if editRR.Code != http.StatusOK {
		t.Fatalf("editor GET /edit status = %d, want 200", editRR.Code)
	}
	editBody := editRR.Body.String()
	for _, want := range []string{
		`<!DOCTYPE html>`,
		`<html lang="en">`,
		`<meta name="viewport" content="width=device-width, initial-scale=1">`,
		`<header `,
		`<nav `,
		`<main `,
		`<label for="editor"`,
		`href="/?date=2026-06-15"`,
		`:focus-visible`,
		`prefers-reduced-motion`,
	} {
		if !strings.Contains(editBody, want) {
			t.Errorf("GET /edit missing expected accessibility/layout marker %q", want)
		}
	}
	csrf := extractCSRF(t, editBody)

	// Editor posts /update with XSS payload
	form := url.Values{
		"date":       {"2026-06-15"},
		"csrf_token": {csrf},
		"contents":   {`<p>Buy milk</p><script>alert(1)</script>`},
	}
	postReq := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(form.Encode()))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.Header.Set("X-Webauth-User", "editor@example.com")
	postRR := httptest.NewRecorder()
	h.ServeHTTP(postRR, postReq)
	if postRR.Code != http.StatusSeeOther {
		t.Fatalf("editor POST /update status = %d, want 303 (body: %s)", postRR.Code, postRR.Body.String())
	}
	if regen.calledDate != "2026-06-15" {
		t.Errorf("regenerator date = %q, want 2026-06-15", regen.calledDate)
	}

	saved, err := os.ReadFile(cfg.ContentPath("2026-06-15"))
	if err != nil {
		t.Fatalf("read saved note: %v", err)
	}
	if strings.Contains(string(saved), "<script") || !strings.Contains(string(saved), "<p>Buy milk</p>") {
		t.Errorf("unexpected sanitized note: %q", string(saved))
	}
}

func TestUpdateSecurityGuards(t *testing.T) {
	regen := &stubRegen{err: errors.New("/secret/path/internal-helper: stderr leak")}
	srv, _ := newTestServer(t, regen)
	h := srv.Handler()

	// GET /update rejected with 405
	getReq := httptest.NewRequest(http.MethodGet, "/update", nil)
	getReq.Header.Set("X-Webauth-User", "admin@example.com")
	getRR := httptest.NewRecorder()
	h.ServeHTTP(getRR, getReq)
	if getRR.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /update status = %d, want 405", getRR.Code)
	}

	// Missing CSRF rejected with 403
	badForm := url.Values{"date": {"2026-06-15"}, "contents": {"hi"}}
	csrfReq := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(badForm.Encode()))
	csrfReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	csrfReq.Header.Set("X-Webauth-User", "admin@example.com")
	csrfRR := httptest.NewRecorder()
	h.ServeHTTP(csrfRR, csrfReq)
	if csrfRR.Code != http.StatusForbidden {
		t.Errorf("missing CSRF status = %d, want 403", csrfRR.Code)
	}

	// Oversized body rejected with 413
	bigReq := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(strings.Repeat("a", maxUpdateBodyBytes+64)))
	bigReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	bigReq.Header.Set("X-Webauth-User", "admin@example.com")
	bigRR := httptest.NewRecorder()
	h.ServeHTTP(bigRR, bigReq)
	if bigRR.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body status = %d, want 413", bigRR.Code)
	}

	// Regenerator error does not leak internal paths
	csrf, err := srv.issueCSRFToken()
	if err != nil {
		t.Fatalf("issueCSRFToken: %v", err)
	}
	okForm := url.Values{"date": {"2026-06-15"}, "csrf_token": {csrf}, "contents": {"hello"}}
	errReq := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(okForm.Encode()))
	errReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	errReq.Header.Set("X-Webauth-User", "admin@example.com")
	errRR := httptest.NewRecorder()
	h.ServeHTTP(errRR, errReq)
	if errRR.Code != http.StatusInternalServerError {
		t.Errorf("regen failure status = %d, want 500", errRR.Code)
	}
	if strings.Contains(errRR.Body.String(), "/secret/path") {
		t.Errorf("HTTP response leaked internal path: %q", errRR.Body.String())
	}

	// Editor blocked from /newtoken (admin-only)
	tokReq := httptest.NewRequest(http.MethodGet, "/newtoken", nil)
	tokReq.Header.Set("X-Webauth-User", "editor@example.com")
	tokRR := httptest.NewRecorder()
	h.ServeHTTP(tokRR, tokReq)
	if tokRR.Code != http.StatusForbidden {
		t.Errorf("editor /newtoken status = %d, want 403", tokRR.Code)
	}
}

func TestDatePathTraversalAndNoFileCreationOnGet(t *testing.T) {
	srv, cfg := newTestServer(t, &stubRegen{})
	h := srv.Handler()

	// Path traversal rejected
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/?date=../../etc/passwd", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("path traversal status = %d, want 400", rr.Code)
	}

	// Future date GET does not create files on disk
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/?date=2099-01-01", nil))
	if rr2.Code != http.StatusOK {
		t.Fatalf("future date GET status = %d, want 200", rr2.Code)
	}
	entries, err := os.ReadDir(cfg.ContentsDir())
	if err != nil {
		t.Fatalf("readdir contents: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 files created on GET, found %d", len(entries))
	}
}

func TestAssetRoutes(t *testing.T) {
	srv, cfg := newTestServer(t, &stubRegen{})
	h := srv.Handler()

	today := "2026-06-15"
	tomorrow := "2026-06-16"
	files := map[string]string{
		cfg.ImagePath(today):            "img-today",
		cfg.HashPath(today):             "sha-today",
		cfg.ImagePath(tomorrow):         "img-tomorrow",
		cfg.HashPath(tomorrow):          "sha-tomorrow",
		cfg.ScreenshotPath(today):       "png-today",
		cfg.ScreenshotPath(tomorrow):    "png-tomorrow",
		cfg.WindowScreenshotPath(today): "win-today",
	}
	for p, content := range files {
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	routes := []struct {
		path string
		want string
	}{
		{"/img", "img-today"},
		{"/img.sha", "sha-today"},
		{"/img2", "img-tomorrow"},
		{"/img2.sha", "sha-tomorrow"},
		{"/screenshot", "png-today"},
		{"/screenshot2", "png-tomorrow"},
		{"/window_screenshot", "win-today"},
		{"/ckengine5.css", ".ck-content"},
	}

	for _, tc := range routes {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rr.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200", tc.path, rr.Code)
		}
		if !strings.Contains(rr.Body.String(), tc.want) {
			t.Errorf("GET %s body = %q, want substring %q", tc.path, rr.Body.String(), tc.want)
		}
	}
}
