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
	assertSecurityHeaders(t, "GET /", rr)
	if ct := rr.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("GET / Content-Type = %q, want text/html; charset=utf-8", ct)
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
	if err := os.WriteFile(cfg.ContentPath("2026-06-15"), []byte(`<p>Existing</p><script>evil()</script>`), 0600); err != nil {
		t.Fatalf("seed note: %v", err)
	}
	editReq := httptest.NewRequest(http.MethodGet, "/edit?date=2026-06-15", nil)
	editReq.Header.Set("X-Webauth-User", "editor@example.com")
	editRR := httptest.NewRecorder()
	h.ServeHTTP(editRR, editReq)
	if editRR.Code != http.StatusOK {
		t.Fatalf("editor GET /edit status = %d, want 200", editRR.Code)
	}
	assertSecurityHeaders(t, "GET /edit", editRR)
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
		`ckeditor5/41.4.2/super-build/ckeditor.js`,
		`integrity="sha384-`,
		`crossorigin="anonymous"`,
		`showPreviews: false`,
		`pageshow`,
	} {
		if !strings.Contains(editBody, want) {
			t.Errorf("GET /edit missing expected accessibility/layout/security marker %q", want)
		}
	}
	if strings.Contains(editBody, "evil()") {
		t.Error("GET /edit rendered unsanitized script from note file")
	}
	csrf := extractCSRF(t, editBody)

	// Another user (admin) cannot use editor's CSRF token.
	crossForm := url.Values{
		"date":       {"2026-06-15"},
		"csrf_token": {csrf},
		"contents":   {`<p>Hijack</p>`},
	}
	crossReq := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(crossForm.Encode()))
	crossReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	crossReq.Header.Set("X-Webauth-User", "admin@example.com")
	crossRR := httptest.NewRecorder()
	h.ServeHTTP(crossRR, crossReq)
	if crossRR.Code != http.StatusForbidden {
		t.Errorf("cross-user CSRF status = %d, want 403", crossRR.Code)
	}

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

	// Non-GET methods rejected on GET-only routes (405)
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/img"},
		{http.MethodPost, "/"},
		{http.MethodPost, "/edit"},
		{http.MethodHead, "/newtoken"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("X-Webauth-User", "admin@example.com")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s status = %d, want 405", tc.method, tc.path, rr.Code)
		}
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
		path        string
		want        string
		wantCT      string
		wantNoStore bool
	}{
		{"/img", "img-today", "application/octet-stream", true},
		{"/img.sha", "sha-today", "text/plain; charset=utf-8", true},
		{"/img2", "img-tomorrow", "application/octet-stream", true},
		{"/img2.sha", "sha-tomorrow", "text/plain; charset=utf-8", true},
		{"/screenshot", "png-today", "image/png", true},
		{"/screenshot2", "png-tomorrow", "image/png", true},
		{"/window_screenshot", "win-today", "image/png", true},
		{"/ckengine5.css", ".ck-content", "text/css; charset=utf-8", false},
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
		if gotCT := rr.Header().Get("Content-Type"); gotCT != tc.wantCT {
			t.Errorf("GET %s Content-Type = %q, want %q", tc.path, gotCT, tc.wantCT)
		}
		if gotNoSniff := rr.Header().Get("X-Content-Type-Options"); gotNoSniff != "nosniff" {
			t.Errorf("GET %s X-Content-Type-Options = %q, want nosniff", tc.path, gotNoSniff)
		}
		if tc.wantNoStore && rr.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("GET %s Cache-Control = %q, want no-store", tc.path, rr.Header().Get("Cache-Control"))
		}
	}
}

func TestUpdateFutureDateHorizonAndSelectiveRegen(t *testing.T) {
	regen := &stubRegen{}
	srv, cfg := newTestServer(t, regen)
	h := srv.Handler()

	// Date more than 365 days ahead is rejected with 400.
	csrf1, err := srv.issueCSRFToken()
	if err != nil {
		t.Fatalf("issueCSRFToken: %v", err)
	}
	farForm := url.Values{
		"date":       {"2028-01-01"},
		"csrf_token": {csrf1},
		"contents":   {"<p>Far future</p>"},
	}
	farReq := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(farForm.Encode()))
	farReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	farReq.Header.Set("X-Webauth-User", "editor@example.com")
	farRR := httptest.NewRecorder()
	h.ServeHTTP(farRR, farReq)
	if farRR.Code != http.StatusBadRequest {
		t.Fatalf("far-future POST /update status = %d, want 400", farRR.Code)
	}
	if regen.calledDate != "" {
		t.Errorf("expected no regeneration for rejected date, got %q", regen.calledDate)
	}

	// Date 5 days ahead saves note without triggering browser regeneration.
	csrf2, err := srv.issueCSRFToken()
	if err != nil {
		t.Fatalf("issueCSRFToken: %v", err)
	}
	nextWeekForm := url.Values{
		"date":       {"2026-06-20"},
		"csrf_token": {csrf2},
		"contents":   {"<p>Next week</p>"},
	}
	nwReq := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(nextWeekForm.Encode()))
	nwReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	nwReq.Header.Set("X-Webauth-User", "editor@example.com")
	nwRR := httptest.NewRecorder()
	h.ServeHTTP(nwRR, nwReq)
	if nwRR.Code != http.StatusSeeOther {
		t.Fatalf("next-week POST /update status = %d, want 303", nwRR.Code)
	}
	if regen.calledDate != "" {
		t.Errorf("expected no regeneration for date > tomorrow, got %q", regen.calledDate)
	}
	saved, err := os.ReadFile(cfg.ContentPath("2026-06-20"))
	if err != nil || !strings.Contains(string(saved), "<p>Next week</p>") {
		t.Errorf("expected next-week note saved, err=%v content=%q", err, string(saved))
	}

	// Tomorrow triggers regeneration.
	csrf3, err := srv.issueCSRFToken()
	if err != nil {
		t.Fatalf("issueCSRFToken: %v", err)
	}
	tmrForm := url.Values{
		"date":       {"2026-06-16"},
		"csrf_token": {csrf3},
		"contents":   {"<p>Tomorrow</p>"},
	}
	tmrReq := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(tmrForm.Encode()))
	tmrReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tmrReq.Header.Set("X-Webauth-User", "editor@example.com")
	tmrRR := httptest.NewRecorder()
	h.ServeHTTP(tmrRR, tmrReq)
	if tmrRR.Code != http.StatusSeeOther {
		t.Fatalf("tomorrow POST /update status = %d, want 303", tmrRR.Code)
	}
	if regen.calledDate != "2026-06-16" {
		t.Errorf("regenerator date = %q, want 2026-06-16", regen.calledDate)
	}
}

func assertSecurityHeaders(t *testing.T, label string, rr *httptest.ResponseRecorder) {
	t.Helper()
	wants := map[string]string{
		"Cache-Control":           "no-store",
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "no-referrer",
		"Content-Security-Policy": "frame-ancestors 'none'",
	}
	for k, want := range wants {
		if got := rr.Header().Get(k); got != want {
			t.Errorf("%s header %s = %q, want %q", label, k, got, want)
		}
	}
}

func TestCSRFTokenMapBounded(t *testing.T) {
	srv, _ := newTestServer(t, &stubRegen{})
	for i := 0; i < maxCSRFTokens+25; i++ {
		if _, err := srv.issueCSRFTokenForUser("editor@example.com"); err != nil {
			t.Fatalf("issueCSRFTokenForUser: %v", err)
		}
	}
	srv.csrfMu.Lock()
	gotLen := len(srv.csrfTokens)
	srv.csrfMu.Unlock()
	if gotLen > maxCSRFTokens {
		t.Errorf("csrfTokens size = %d, want <= %d", gotLen, maxCSRFTokens)
	}
}
