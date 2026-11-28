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
	html            string
	err             error
	expiry          string
	redirectCalled  *bool
	saveTokenCalled *bool
}

func (s stubCalendar) RenderHTML(_ context.Context, _ time.Time) (string, error) {
	return s.html, s.err
}
func (s stubCalendar) TokenExpiry() string { return s.expiry }
func (s stubCalendar) RedirectForToken(w http.ResponseWriter, _ *http.Request) {
	if s.redirectCalled != nil {
		*s.redirectCalled = true
	}
	w.WriteHeader(http.StatusSeeOther)
}
func (s stubCalendar) ProcessAuthCode(w http.ResponseWriter, _ *http.Request) {
	if s.saveTokenCalled != nil {
		*s.saveTokenCalled = true
	}
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
		Transit:     stubTransit{lines: []transit.LineDepartures{{Line: "9", Next: "10:05"}, {Line: "14", Next: "10:10"}, {Line: "32", Next: "10:15"}}},
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

func TestNewServerGuardsConfigAndDefaultsLocation(t *testing.T) {
	t.Run("nil Config panics", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic when deps.Config is nil")
			}
		}()
		NewServer(Dependencies{})
	})

	t.Run("nil Location defaults to UTC without mutating caller config", func(t *testing.T) {
		cfg := &config.Config{DataDir: t.TempDir()}
		srv := NewServer(Dependencies{Config: cfg})
		if srv.cfg.Location != time.UTC {
			t.Errorf("srv.cfg.Location = %v, want UTC", srv.cfg.Location)
		}
		if cfg.Location != nil {
			t.Errorf("caller cfg.Location was mutated to %v", cfg.Location)
		}
	})
}

func TestViewComputesServerLinksAndPreservesDimensions(t *testing.T) {
	srv, cfg := newTestServer(t, &stubRegen{})
	h := srv.Handler()

	// Seed RefreshTokenFile to verify elapsed display.
	refTime := time.Date(2026, 6, 15, 7, 30, 0, 0, cfg.Location)
	if err := os.WriteFile(cfg.RefreshTokenFile(), nil, 0600); err != nil {
		t.Fatalf("write refresh token marker: %v", err)
	}
	if err := os.Chtimes(cfg.RefreshTokenFile(), refTime, refTime); err != nil {
		t.Fatalf("chtimes refresh token marker: %v", err)
	}

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
	if strings.Contains(body, "#ww_2315453e4f5da") {
		t.Error("view.tmpl still contains hardcoded #ww_2315453e4f5da selector")
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
		`<div class="weather-widget">`,
		`.weather-widget [id^="ww_"]`,
		`(last 2026-06-15T07:30:00&#43;02:00, elapsed 02:30)`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("GET / missing expected marker %q", want)
		}
	}
	// Transit spacing: 3 lines should have 2 divs with padding-top: 5px.
	if got := strings.Count(body, `style="padding-top: 5px;"`); got != 2 {
		t.Errorf("transit padding-top count = %d, want 2", got)
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

	// Yesterday: rejected with 400 and security headers
	rr3 := httptest.NewRecorder()
	h.ServeHTTP(rr3, httptest.NewRequest(http.MethodGet, "/?date=2026-06-14", nil))
	if rr3.Code != http.StatusBadRequest {
		t.Errorf("prior day status = %d, want 400", rr3.Code)
	}
	assertSecurityHeaders(t, "GET / prior day 400", rr3)
}

func TestViewAndEditHorizonLimit(t *testing.T) {
	srv, _ := newTestServer(t, &stubRegen{})
	h := srv.Handler()

	// Exactly +365 days (2026-06-15 + 365 days = 2027-06-15): allowed on / and /edit; Next link hidden on /.
	rrAtMax := httptest.NewRecorder()
	h.ServeHTTP(rrAtMax, httptest.NewRequest(http.MethodGet, "/?date=2027-06-15", nil))
	if rrAtMax.Code != http.StatusOK {
		t.Fatalf("GET / at +365 days status = %d, want 200", rrAtMax.Code)
	}
	if strings.Contains(rrAtMax.Body.String(), `id="nextdaylink"`) {
		t.Error("expected Next link hidden at +365 days")
	}

	editAtMaxReq := httptest.NewRequest(http.MethodGet, "/edit?date=2027-06-15", nil)
	editAtMaxReq.Header.Set("X-Webauth-User", "editor@example.com")
	editAtMaxRR := httptest.NewRecorder()
	h.ServeHTTP(editAtMaxRR, editAtMaxReq)
	if editAtMaxRR.Code != http.StatusOK {
		t.Fatalf("GET /edit at +365 days status = %d, want 200", editAtMaxRR.Code)
	}

	// +366 days (2027-06-16): rejected with 400 on / and /edit.
	rrOver := httptest.NewRecorder()
	h.ServeHTTP(rrOver, httptest.NewRequest(http.MethodGet, "/?date=2027-06-16", nil))
	if rrOver.Code != http.StatusBadRequest {
		t.Errorf("GET / at +366 days status = %d, want 400", rrOver.Code)
	}

	editOverReq := httptest.NewRequest(http.MethodGet, "/edit?date=2027-06-16", nil)
	editOverReq.Header.Set("X-Webauth-User", "editor@example.com")
	editOverRR := httptest.NewRecorder()
	h.ServeHTTP(editOverRR, editOverReq)
	if editOverRR.Code != http.StatusBadRequest {
		t.Errorf("GET /edit at +366 days status = %d, want 400", editOverRR.Code)
	}
}

func TestReadOnlyHandlerExposesOnlyDisplayAndAssets(t *testing.T) {
	srv, cfg := newTestServer(t, &stubRegen{})
	ro := srv.ReadOnlyHandler()

	today := "2026-06-15"
	if err := os.WriteFile(cfg.ImagePath(today), []byte("img-bytes"), 0600); err != nil {
		t.Fatalf("write img: %v", err)
	}

	for _, path := range []string{"/", "/ckengine5.css", "/img"} {
		rr := httptest.NewRecorder()
		ro.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Errorf("ReadOnlyHandler GET %s status = %d, want 200", path, rr.Code)
		}
	}

	blocked := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/edit"},
		{http.MethodPost, "/update"},
		{http.MethodGet, "/newtoken"},
		{http.MethodGet, "/savetoken"},
	}
	for _, tc := range blocked {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("X-Webauth-User", "admin@example.com")
		rr := httptest.NewRecorder()
		ro.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Errorf("ReadOnlyHandler %s %s status = %d, want 404", tc.method, tc.path, rr.Code)
		}
		assertSecurityHeaders(t, "ReadOnlyHandler 404 "+tc.path, rr)
	}
}

func TestNilOptionalDepsAndCalendarErrorFallback(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Parse([]byte(`{
		"data_dir": "`+dir+`",
		"admin_users": ["admin@example.com"]
	}`), "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	authz := auth.NewAuthorizer(cfg.AdminUsers, nil, auth.HeaderResolver{})

	t.Run("nil calendar transit regenerator and Now", func(t *testing.T) {
		srv := NewServer(Dependencies{
			Config:     cfg,
			Authorizer: authz,
		})
		h := srv.Handler()

		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET / status = %d, want 200", rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "Access token expires: unknown") {
			t.Errorf("expected unknown token expiry, body: %s", rr.Body.String())
		}

		for _, path := range []string{"/newtoken", "/savetoken"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("X-Webauth-User", "admin@example.com")
			tokRR := httptest.NewRecorder()
			h.ServeHTTP(tokRR, req)
			if tokRR.Code != http.StatusInternalServerError {
				t.Errorf("GET %s with nil calendar status = %d, want 500", path, tokRR.Code)
			}
			assertSecurityHeaders(t, "GET "+path+" 500", tokRR)
		}
	})

	t.Run("calendar error renders fallback and empty expiry renders unknown", func(t *testing.T) {
		srv := NewServer(Dependencies{
			Config:     cfg,
			Authorizer: authz,
			Calendar:   stubCalendar{err: errors.New("oauth expired"), expiry: ""},
		})
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET / status = %d, want 200", rr.Code)
		}
		body := rr.Body.String()
		if !strings.Contains(body, "Calendar unavailable") {
			t.Error("expected Calendar unavailable fallback")
		}
		if !strings.Contains(body, "Access token expires: unknown") {
			t.Error("expected unknown when TokenExpiry returns empty string")
		}
	})
}

func TestHandleNewTokenAndSaveToken(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Parse([]byte(`{
		"data_dir": "`+dir+`",
		"admin_users": ["admin@example.com"],
		"editor_users": ["editor@example.com"]
	}`), "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	authz := auth.NewAuthorizer(cfg.AdminUsers, cfg.EditorUsers, auth.HeaderResolver{})
	redirectCalled := false
	saveTokenCalled := false
	srv := NewServer(Dependencies{
		Config:     cfg,
		Authorizer: authz,
		Calendar: stubCalendar{
			redirectCalled:  &redirectCalled,
			saveTokenCalled: &saveTokenCalled,
		},
	})
	h := srv.Handler()

	// Admin GET /newtoken succeeds.
	req := httptest.NewRequest(http.MethodGet, "/newtoken", nil)
	req.Header.Set("X-Webauth-User", "admin@example.com")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || !redirectCalled {
		t.Errorf("GET /newtoken status=%d called=%v, want 303/true", rr.Code, redirectCalled)
	}

	// HEAD /savetoken rejected with 405 and does not invoke ProcessAuthCode.
	headReq := httptest.NewRequest(http.MethodHead, "/savetoken?code=abc", nil)
	headReq.Header.Set("X-Webauth-User", "admin@example.com")
	headRR := httptest.NewRecorder()
	h.ServeHTTP(headRR, headReq)
	if headRR.Code != http.StatusMethodNotAllowed {
		t.Errorf("HEAD /savetoken status = %d, want 405", headRR.Code)
	}
	if headRR.Header().Get("Allow") != http.MethodGet {
		t.Errorf("HEAD /savetoken Allow = %q, want GET", headRR.Header().Get("Allow"))
	}
	if saveTokenCalled {
		t.Error("HEAD /savetoken invoked ProcessAuthCode")
	}

	// Editor GET /savetoken rejected with 403 and security headers.
	edReq := httptest.NewRequest(http.MethodGet, "/savetoken?code=abc", nil)
	edReq.Header.Set("X-Webauth-User", "editor@example.com")
	edRR := httptest.NewRecorder()
	h.ServeHTTP(edRR, edReq)
	if edRR.Code != http.StatusForbidden {
		t.Errorf("editor GET /savetoken status = %d, want 403", edRR.Code)
	}
	assertSecurityHeaders(t, "editor /savetoken 403", edRR)

	// Unauthenticated GET /savetoken rejected with 401 and security headers.
	unauthRR := httptest.NewRecorder()
	h.ServeHTTP(unauthRR, httptest.NewRequest(http.MethodGet, "/savetoken?code=abc", nil))
	if unauthRR.Code != http.StatusUnauthorized {
		t.Errorf("unauth GET /savetoken status = %d, want 401", unauthRR.Code)
	}
	assertSecurityHeaders(t, "unauth /savetoken 401", unauthRR)

	// Admin GET /savetoken succeeds.
	okReq := httptest.NewRequest(http.MethodGet, "/savetoken?code=abc", nil)
	okReq.Header.Set("X-Webauth-User", "admin@example.com")
	okRR := httptest.NewRecorder()
	h.ServeHTTP(okRR, okReq)
	if okRR.Code != http.StatusSeeOther || !saveTokenCalled {
		t.Errorf("admin GET /savetoken status=%d called=%v, want 303/true", okRR.Code, saveTokenCalled)
	}
}

func TestEditorCanEditAndUpdateWithCSRFAndSanitization(t *testing.T) {
	regen := &stubRegen{}
	srv, cfg := newTestServer(t, regen)
	h := srv.Handler()

	// HEAD /edit rejected with 405 and does not mint CSRF tokens.
	headEdit := httptest.NewRequest(http.MethodHead, "/edit?date=2026-06-15", nil)
	headEdit.Header.Set("X-Webauth-User", "editor@example.com")
	headEditRR := httptest.NewRecorder()
	h.ServeHTTP(headEditRR, headEdit)
	if headEditRR.Code != http.StatusMethodNotAllowed {
		t.Errorf("HEAD /edit status = %d, want 405", headEditRR.Code)
	}
	if headEditRR.Header().Get("Allow") != http.MethodGet {
		t.Errorf("HEAD /edit Allow = %q, want GET", headEditRR.Header().Get("Allow"))
	}
	srv.csrfMu.Lock()
	tokenCountAfterHead := len(srv.csrfTokens)
	srv.csrfMu.Unlock()
	if tokenCountAfterHead != 0 {
		t.Errorf("HEAD /edit minted %d CSRF tokens, want 0", tokenCountAfterHead)
	}

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
		`pageshow`,
		`window.location.reload()`,
	} {
		if !strings.Contains(editBody, want) {
			t.Errorf("GET /edit missing expected accessibility/layout/security marker %q", want)
		}
	}
	for _, unwanted := range []string{"evil()", "mediaEmbed", "htmlEmbed", "toggleDownloadable"} {
		if strings.Contains(editBody, unwanted) {
			t.Errorf("GET /edit unexpectedly contains %q", unwanted)
		}
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

	// Bad form submission (missing contents) returns 400 and preserves CSRF token for retry.
	badFieldsForm := url.Values{
		"date":       {"2026-06-15"},
		"csrf_token": {csrf},
	}
	badFieldsReq := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(badFieldsForm.Encode()))
	badFieldsReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	badFieldsReq.Header.Set("X-Webauth-User", "editor@example.com")
	badFieldsRR := httptest.NewRecorder()
	h.ServeHTTP(badFieldsRR, badFieldsReq)
	if badFieldsRR.Code != http.StatusBadRequest {
		t.Errorf("missing contents status = %d, want 400", badFieldsRR.Code)
	}

	// Editor posts /update with XSS payload using the still-valid CSRF token.
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

	// Replaying the consumed CSRF token fails with 403.
	replayReq := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(form.Encode()))
	replayReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	replayReq.Header.Set("X-Webauth-User", "editor@example.com")
	replayRR := httptest.NewRecorder()
	h.ServeHTTP(replayRR, replayReq)
	if replayRR.Code != http.StatusForbidden {
		t.Errorf("replayed CSRF status = %d, want 403", replayRR.Code)
	}

	saved, err := os.ReadFile(cfg.ContentPath("2026-06-15"))
	if err != nil {
		t.Fatalf("read saved note: %v", err)
	}
	if strings.Contains(string(saved), "<script") || !strings.Contains(string(saved), "<p>Buy milk</p>") {
		t.Errorf("unexpected sanitized note: %q", string(saved))
	}
}

func TestUpdateSecurityGuardsAndEdgeCases(t *testing.T) {
	regen := &stubRegen{err: errors.New("/secret/path/internal-helper: stderr leak")}
	srv, cfg := newTestServer(t, regen)
	h := srv.Handler()

	// Empty user cannot mint or consume CSRF tokens.
	if _, err := srv.issueCSRFTokenForUser(""); err == nil {
		t.Error("issueCSRFTokenForUser(\"\") succeeded, want error")
	}
	validTok, err := srv.issueCSRFTokenForUser("admin@example.com")
	if err != nil {
		t.Fatalf("issueCSRFTokenForUser: %v", err)
	}
	if srv.consumeCSRFTokenForUser(validTok, "") {
		t.Error("consumeCSRFTokenForUser with empty user succeeded, want false")
	}

	// GET /update rejected with 405
	getReq := httptest.NewRequest(http.MethodGet, "/update", nil)
	getReq.Header.Set("X-Webauth-User", "admin@example.com")
	getRR := httptest.NewRecorder()
	h.ServeHTTP(getRR, getReq)
	if getRR.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /update status = %d, want 405", getRR.Code)
	}
	assertSecurityHeaders(t, "GET /update 405", getRR)

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
	assertSecurityHeaders(t, "POST /update missing CSRF 403", csrfRR)

	// Oversized body rejected with 413
	bigReq := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(strings.Repeat("a", maxUpdateBodyBytes+64)))
	bigReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	bigReq.Header.Set("X-Webauth-User", "admin@example.com")
	bigRR := httptest.NewRecorder()
	h.ServeHTTP(bigRR, bigReq)
	if bigRR.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body status = %d, want 413", bigRR.Code)
	}

	// Validation edge cases with valid CSRF: missing date, invalid date, prior day.
	for _, tc := range []struct {
		name string
		form url.Values
	}{
		{"missing date", url.Values{"csrf_token": {validTok}, "contents": {"hi"}}},
		{"invalid date", url.Values{"csrf_token": {validTok}, "date": {"not-a-date"}, "contents": {"hi"}}},
		{"prior day", url.Values{"csrf_token": {validTok}, "date": {"2026-06-14"}, "contents": {"hi"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(tc.form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("X-Webauth-User", "admin@example.com")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Errorf("%s status = %d, want 400", tc.name, rr.Code)
			}
		})
	}

	// Regenerator error saves note, informs user note was saved, and does not leak internal paths.
	okForm := url.Values{"date": {"2026-06-15"}, "csrf_token": {validTok}, "contents": {"<p>Saved note</p>"}}
	errReq := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(okForm.Encode()))
	errReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	errReq.Header.Set("X-Webauth-User", "admin@example.com")
	errRR := httptest.NewRecorder()
	h.ServeHTTP(errRR, errReq)
	if errRR.Code != http.StatusInternalServerError {
		t.Errorf("regen failure status = %d, want 500", errRR.Code)
	}
	assertSecurityHeaders(t, "POST /update regen failure 500", errRR)
	if strings.Contains(errRR.Body.String(), "/secret/path") {
		t.Errorf("HTTP response leaked internal path: %q", errRR.Body.String())
	}
	if !strings.Contains(errRR.Body.String(), "The server saved the note, but failed to update the display image") {
		t.Errorf("regen failure body = %q, want indication that the server saved the note", errRR.Body.String())
	}
	saved, err := os.ReadFile(cfg.ContentPath("2026-06-15"))
	if err != nil || string(saved) != "<p>Saved note</p>" {
		t.Errorf("note on disk after regen error = (%q, %v), want <p>Saved note</p>", string(saved), err)
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
		{http.MethodHead, "/edit"},
		{http.MethodHead, "/newtoken"},
		{http.MethodHead, "/savetoken"},
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

	// Valid future date within 365 days does not create files on disk
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/?date=2026-07-01", nil))
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
		wantDate    string
	}{
		{"/img", "img-today", "application/octet-stream", true, today},
		{"/img.sha", "sha-today", "text/plain; charset=utf-8", true, today},
		{"/img2", "img-tomorrow", "application/octet-stream", true, tomorrow},
		{"/img2.sha", "sha-tomorrow", "text/plain; charset=utf-8", true, tomorrow},
		{"/screenshot", "png-today", "image/png", true, today},
		{"/screenshot2", "png-tomorrow", "image/png", true, tomorrow},
		{"/window_screenshot", "win-today", "image/png", true, today},
		{"/ckengine5.css", ".ck-content", "text/css; charset=utf-8", false, ""},
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
		if tc.wantDate != "" {
			if gotDate := rr.Header().Get("X-Image-Date"); gotDate != tc.wantDate {
				t.Errorf("GET %s X-Image-Date = %q, want %q", tc.path, gotDate, tc.wantDate)
			}
			if gotTZ := rr.Header().Get("X-Server-Timezone"); gotTZ != "Europe/Zurich" {
				t.Errorf("GET %s X-Server-Timezone = %q, want Europe/Zurich", tc.path, gotTZ)
			}
		}
	}
}

func TestUpdateFutureDateHorizonAndSelectiveRegen(t *testing.T) {
	regen := &stubRegen{}
	srv, cfg := newTestServer(t, regen)
	h := srv.Handler()

	// Date more than 365 days ahead is rejected with 400.
	csrf1, err := srv.issueCSRFTokenForUser("editor@example.com")
	if err != nil {
		t.Fatalf("issueCSRFTokenForUser: %v", err)
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
	csrf2, err := srv.issueCSRFTokenForUser("editor@example.com")
	if err != nil {
		t.Fatalf("issueCSRFTokenForUser: %v", err)
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
	csrf3, err := srv.issueCSRFTokenForUser("editor@example.com")
	if err != nil {
		t.Fatalf("issueCSRFTokenForUser: %v", err)
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
		"Content-Security-Policy": baselineCSP,
	}
	for k, want := range wants {
		if got := rr.Header().Get(k); got != want {
			t.Errorf("%s header %s = %q, want %q", label, k, got, want)
		}
	}
}

func TestCSRFTokenMapBoundedAndExpiry(t *testing.T) {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	cfg := &config.Config{DataDir: t.TempDir()}
	srv := NewServer(Dependencies{
		Config: cfg,
		Now:    func() time.Time { return now },
	})

	tok, err := srv.issueCSRFTokenForUser("editor@example.com")
	if err != nil {
		t.Fatalf("issueCSRFTokenForUser: %v", err)
	}
	now = now.Add(csrfTokenTTL + time.Minute)
	if srv.verifyCSRFTokenForUser(tok, "editor@example.com") {
		t.Error("expired CSRF token verified as valid")
	}

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
