package calendar

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
	gcal "google.golang.org/api/calendar/v3"
)

type stubFetcher struct {
	fn func(calID, timeMin, timeMax string) ([]*gcal.Event, error)
}

func (s stubFetcher) ListEvents(_ context.Context, calID, timeMin, timeMax string) ([]*gcal.Event, error) {
	return s.fn(calID, timeMin, timeMax)
}

func TestRenderHTMLDedupeSortAndTomorrowSkip(t *testing.T) {
	zurich, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		t.Fatalf("load Europe/Zurich: %v", err)
	}
	now := time.Date(2026, 6, 15, 11, 0, 0, 0, zurich)

	fetcher := stubFetcher{
		fn: func(calID, timeMin, _ string) ([]*gcal.Event, error) {
			if strings.HasPrefix(timeMin, "2026-06-15") {
				if calID == "cal1@example.com" {
					return []*gcal.Event{
						{Id: "dup", Summary: "Lunch", Start: &gcal.EventDateTime{DateTime: "2026-06-15T12:00:00+02:00"}, End: &gcal.EventDateTime{DateTime: "2026-06-15T13:00:00+02:00"}},
						{Id: "past", Summary: "Ended", Start: &gcal.EventDateTime{DateTime: "2026-06-15T08:00:00+02:00"}, End: &gcal.EventDateTime{DateTime: "2026-06-15T09:00:00+02:00"}},
						{Id: "allday", Summary: "Holiday & Fun", Start: &gcal.EventDateTime{Date: "2026-06-15"}},
						{Id: "badtime", Summary: "Corrupt", Start: &gcal.EventDateTime{DateTime: "not-a-time"}},
						{Id: "nilstart", Summary: "NoStart", Start: nil},
					}, nil
				}
				return []*gcal.Event{
					{Id: "dup", Summary: "Lunch", Start: &gcal.EventDateTime{DateTime: "2026-06-15T12:00:00+02:00"}, End: &gcal.EventDateTime{DateTime: "2026-06-15T13:00:00+02:00"}},
				}, nil
			}
			// Tomorrow has only an event with invalid start timestamp -> filtered out
			return []*gcal.Event{
				{Id: "tmr-bad", Summary: "BadTomorrow", Start: &gcal.EventDateTime{DateTime: "invalid"}},
			}, nil
		},
	}

	svc, err := NewService(context.Background(), Options{
		Calendars: []string{"cal1@example.com", "cal2@example.com"},
		Location:  zurich,
		Now:       func() time.Time { return now },
		Fetcher:   fetcher,
	})
	if err != nil {
		t.Fatalf("NewService failed: %v", err)
	}

	out, err := svc.RenderHTML(context.Background(), now)
	if err != nil {
		t.Fatalf("RenderHTML failed: %v", err)
	}

	if !strings.Contains(out, "<li>Holiday &amp; Fun</li>") {
		t.Errorf("expected escaped all-day event, got %q", out)
	}
	if strings.Count(out, "12:00 - 13:00: Lunch") != 1 {
		t.Errorf("expected deduplicated Lunch event once, got %q", out)
	}
	if strings.Contains(out, "Ended") {
		t.Errorf("expected past event filtered out, got %q", out)
	}
	if strings.Contains(out, "Tomorrow ...") {
		t.Errorf("expected Tomorrow header skipped when all tomorrow events filtered out, got %q", out)
	}
}

func TestRenderHTMLDSTFallbackMultiDayDedupeAndEndlessEvents(t *testing.T) {
	zurich, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		t.Fatalf("load Europe/Zurich: %v", err)
	}

	// 2026-10-24 22:30 UTC is 2026-10-25 00:30 CEST (start of the 25-hour DST fall-back day).
	// Naive t.AddDate(0, 0, 1) in UTC gives 2026-10-25 22:30 UTC = 2026-10-25 23:30 CET (same local day).
	utcTarget := time.Date(2026, 10, 24, 22, 30, 0, 0, time.UTC)
	now := time.Date(2026, 10, 25, 10, 0, 0, 0, zurich)

	var queriedMins []string
	fetcher := stubFetcher{
		fn: func(_, timeMin, _ string) ([]*gcal.Event, error) {
			queriedMins = append(queriedMins, timeMin)
			if strings.HasPrefix(timeMin, "2026-10-25") {
				return []*gcal.Event{
					{Id: "conf", Summary: "Multi-Day Conf", Start: &gcal.EventDateTime{Date: "2026-10-25"}, End: &gcal.EventDateTime{Date: "2026-10-27"}},
					{Id: "overnight", Summary: "Night Shift", Start: &gcal.EventDateTime{DateTime: "2026-10-25T22:00:00+01:00"}, End: &gcal.EventDateTime{DateTime: "2026-10-26T06:00:00+01:00"}},
					{Id: "endless-past", Summary: "Old Reminder", Start: &gcal.EventDateTime{DateTime: "2026-10-25T09:00:00+01:00"}},
					{Id: "endless-future", Summary: "Future Reminder", Start: &gcal.EventDateTime{DateTime: "2026-10-25T15:00:00+01:00"}},
				}, nil
			}
			if strings.HasPrefix(timeMin, "2026-10-26") {
				return []*gcal.Event{
					{Id: "conf", Summary: "Multi-Day Conf", Start: &gcal.EventDateTime{Date: "2026-10-25"}, End: &gcal.EventDateTime{Date: "2026-10-27"}},
					{Id: "overnight", Summary: "Night Shift", Start: &gcal.EventDateTime{DateTime: "2026-10-25T22:00:00+01:00"}, End: &gcal.EventDateTime{DateTime: "2026-10-26T06:00:00+01:00"}},
					{Id: "tmr-only", Summary: "Monday Sync", Start: &gcal.EventDateTime{DateTime: "2026-10-26T09:00:00+01:00"}, End: &gcal.EventDateTime{DateTime: "2026-10-26T10:00:00+01:00"}},
				}, nil
			}
			return nil, nil
		},
	}

	svc, err := NewService(context.Background(), Options{
		Calendars: []string{"primary"},
		Location:  zurich,
		Now:       func() time.Time { return now },
		Fetcher:   fetcher,
	})
	if err != nil {
		t.Fatalf("NewService failed: %v", err)
	}

	out, err := svc.RenderHTML(context.Background(), utcTarget)
	if err != nil {
		t.Fatalf("RenderHTML failed: %v", err)
	}

	if len(queriedMins) != 2 || !strings.HasPrefix(queriedMins[0], "2026-10-25") || !strings.HasPrefix(queriedMins[1], "2026-10-26") {
		t.Fatalf("queried timeMins = %v, want 2026-10-25 then 2026-10-26", queriedMins)
	}
	if strings.Contains(out, "Old Reminder") {
		t.Errorf("end-less past timed event should be skipped: %q", out)
	}
	if !strings.Contains(out, "<li>15:00: Future Reminder</li>") {
		t.Errorf("end-less future timed event missing: %q", out)
	}
	if strings.Count(out, "Multi-Day Conf") != 1 {
		t.Errorf("multi-day event duplicated across tomorrow: %q", out)
	}
	if strings.Count(out, "Night Shift") != 1 {
		t.Errorf("overnight event duplicated across tomorrow: %q", out)
	}
	if !strings.Contains(out, "Monday Sync") || !strings.Contains(out, "Tomorrow ...") {
		t.Errorf("expected Tomorrow section with Monday Sync, got %q", out)
	}
}

func TestEventsPartialFailureAndNilReceiver(t *testing.T) {
	var nilSvc *Service
	if _, err := nilSvc.Events(context.Background(), time.Now()); err == nil {
		t.Fatal("expected error for nil receiver Events")
	}

	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	svc, err := NewService(context.Background(), Options{
		Calendars: []string{"fail@example.com", "ok@example.com"},
		Location:  time.UTC,
		Fetcher: stubFetcher{
			fn: func(calID, _, _ string) ([]*gcal.Event, error) {
				if calID == "fail@example.com" {
					return nil, errors.New("upstream 503")
				}
				return []*gcal.Event{
					{Id: "ok-1", Summary: "Standup", Start: &gcal.EventDateTime{Date: "2026-06-15"}},
				}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewService failed: %v", err)
	}

	events, err := svc.Events(context.Background(), now)
	if err != nil {
		t.Fatalf("partial failure should succeed, got %v", err)
	}
	if len(events) != 1 || events[0].Id != "ok-1" {
		t.Errorf("events = %+v, want [ok-1]", events)
	}
}

func TestEventsAllCalendarsFailReturnsError(t *testing.T) {
	svc, err := NewService(context.Background(), Options{
		Calendars: []string{"cal1@example.com", "cal2@example.com"},
		Location:  time.UTC,
		Fetcher: stubFetcher{
			fn: func(_, _, _ string) ([]*gcal.Event, error) {
				return nil, errors.New("upstream 503")
			},
		},
	})
	if err != nil {
		t.Fatalf("NewService failed: %v", err)
	}

	if _, err := svc.Events(context.Background(), time.Now()); err == nil {
		t.Fatal("expected error when all calendar calls fail, got nil")
	}
}

func TestOAuthStateValidationAndExpiry(t *testing.T) {
	currentTime := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	svc, err := NewService(context.Background(), Options{
		Location: time.UTC,
		Now:      func() time.Time { return currentTime },
		Fetcher:  stubFetcher{fn: func(_, _, _ string) ([]*gcal.Event, error) { return nil, nil }},
	})
	if err != nil {
		t.Fatalf("NewService failed: %v", err)
	}

	svc.mu.Lock()
	svc.config = &oauth2.Config{
		ClientID:    "test-id",
		RedirectURL: "https://example.com/savetoken",
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://accounts.example.com/auth",
			TokenURL: "https://accounts.example.com/token",
		},
	}
	svc.mu.Unlock()

	// Issue state via RedirectForToken
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/newtoken", nil)
	svc.RedirectForToken(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("RedirectForToken status = %d, want 303", rr.Code)
	}
	locURL, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location header: %v", err)
	}
	state := locURL.Query().Get("state")
	if len(state) != oauthStateBytes*2 {
		t.Fatalf("state len = %d, want %d", len(state), oauthStateBytes*2)
	}
	if locURL.Query().Get("prompt") != "consent" {
		t.Errorf("expected prompt=consent, got %q", locURL.RawQuery)
	}
	if locURL.Query().Get("code_challenge_method") != "S256" || locURL.Query().Get("code_challenge") == "" {
		t.Errorf("expected PKCE S256 challenge in auth URL, got %q", locURL.RawQuery)
	}
	cookies := rr.Result().Cookies()
	var stateCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == oauthStateCookieName {
			stateCookie = c
			break
		}
	}
	if stateCookie == nil || stateCookie.Value != state || !stateCookie.HttpOnly {
		t.Fatalf("expected HttpOnly state cookie matching %q, got %+v", state, stateCookie)
	}

	// Valid state without cookie rejected
	rrNoCookie := httptest.NewRecorder()
	reqNoCookie := httptest.NewRequest(http.MethodGet, "/savetoken?state="+state+"&code=abc", nil)
	svc.ProcessAuthCode(rrNoCookie, reqNoCookie)
	if rrNoCookie.Code != http.StatusBadRequest {
		t.Errorf("missing cookie status = %d, want 400", rrNoCookie.Code)
	}

	// Wrong state rejected
	rrBad := httptest.NewRecorder()
	reqBad := httptest.NewRequest(http.MethodGet, "/savetoken?state=wrong&code=abc", nil)
	reqBad.AddCookie(stateCookie)
	svc.ProcessAuthCode(rrBad, reqBad)
	if rrBad.Code != http.StatusBadRequest {
		t.Errorf("wrong state status = %d, want 400", rrBad.Code)
	}

	// Valid state consumed once
	if !svc.consumeState(state) {
		t.Error("expected issued state to validate")
	}
	if svc.consumeState(state) {
		t.Error("expected state to be single-use")
	}

	// Expired state rejected
	expState, err := svc.issueState()
	if err != nil {
		t.Fatalf("issueState failed: %v", err)
	}
	currentTime = currentTime.Add(oauthStateTTL + time.Second)
	if svc.consumeState(expState) {
		t.Error("expected expired state to be rejected")
	}
}

func TestProcessAuthCodeEndToEndPKCECookiesAndRefreshMarker(t *testing.T) {
	var returnRefresh atomic.Bool
	returnRefresh.Store(true)
	var lastVerifier atomic.Value

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		lastVerifier.Store(r.FormValue("code_verifier"))
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"access_token": "tok-access-1",
			"token_type":   "Bearer",
			"expires_in":   3600,
		}
		if returnRefresh.Load() {
			resp["refresh_token"] = "tok-refresh-1"
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer tokenSrv.Close()

	dir := t.TempDir()
	clientSecretPath := filepath.Join(dir, "client_secret.json")
	tokenPath := filepath.Join(dir, "token.json")
	refreshMarkerPath := filepath.Join(dir, "refresh_tok_obtained.empty")

	clientSecretJSON := fmt.Sprintf(`{"web":{"client_id":"cid","client_secret":"csec","auth_uri":"%s/auth","token_uri":"%s/token","redirect_uris":["https://example.com/savetoken"]}}`, tokenSrv.URL, tokenSrv.URL)
	if err := os.WriteFile(clientSecretPath, []byte(clientSecretJSON), 0600); err != nil {
		t.Fatalf("write client_secret: %v", err)
	}

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	// Start without token.json: NewService returns error, but RedirectForToken must still load OAuth config cleanly.
	svc, err := NewService(context.Background(), Options{
		Location:         time.UTC,
		TokenFile:        tokenPath,
		RefreshTokenFile: refreshMarkerPath,
		ClientSecretFile: clientSecretPath,
		Now:              func() time.Time { return now },
	})
	if err == nil {
		t.Fatal("expected startup error when token.json is absent")
	}
	defer svc.Close()

	// Step 1: RedirectForToken over HTTPS (X-Forwarded-Proto: https)
	rrRedir := httptest.NewRecorder()
	reqRedir := httptest.NewRequest(http.MethodGet, "/newtoken", nil)
	reqRedir.Header.Set("X-Forwarded-Proto", "https")
	svc.RedirectForToken(rrRedir, reqRedir)
	if rrRedir.Code != http.StatusSeeOther {
		t.Fatalf("RedirectForToken status = %d, want 303", rrRedir.Code)
	}
	authURL, err := url.Parse(rrRedir.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	state := authURL.Query().Get("state")
	challenge := authURL.Query().Get("code_challenge")
	var stateCookie *http.Cookie
	for _, c := range rrRedir.Result().Cookies() {
		if c.Name == oauthStateCookieName {
			stateCookie = c
		}
	}
	if stateCookie == nil || !stateCookie.Secure {
		t.Fatalf("expected Secure state cookie on HTTPS, got %+v", stateCookie)
	}

	// Step 2: ProcessAuthCode exchanges code with PKCE verifier, clears Secure cookie, persists token and marker.
	rrCB := httptest.NewRecorder()
	reqCB := httptest.NewRequest(http.MethodGet, "/savetoken?state="+state+"&code=auth-code-1", nil)
	reqCB.Header.Set("X-Forwarded-Proto", "https")
	reqCB.AddCookie(stateCookie)
	svc.ProcessAuthCode(rrCB, reqCB)
	if rrCB.Code != http.StatusSeeOther || rrCB.Header().Get("Location") != "/" {
		t.Fatalf("ProcessAuthCode status=%d loc=%q, want 303 /", rrCB.Code, rrCB.Header().Get("Location"))
	}

	// Verify PKCE verifier hashes to S256 challenge.
	verifier, _ := lastVerifier.Load().(string)
	sum := sha256.Sum256([]byte(verifier))
	if gotChallenge := base64.RawURLEncoding.EncodeToString(sum[:]); gotChallenge != challenge {
		t.Errorf("PKCE verifier mismatch: got challenge %q, want %q", gotChallenge, challenge)
	}

	// Verify state cookie cleared with Secure: true, MaxAge: -1, Expires: Unix(0,0).
	var clearedCookie *http.Cookie
	for _, c := range rrCB.Result().Cookies() {
		if c.Name == oauthStateCookieName {
			clearedCookie = c
		}
	}
	if clearedCookie == nil || !clearedCookie.Secure || clearedCookie.MaxAge != -1 || !clearedCookie.Expires.Equal(time.Unix(0, 0).UTC()) {
		t.Errorf("cleared cookie = %+v, want Secure=true MaxAge=-1 Expires=epoch", clearedCookie)
	}

	// Verify token.json persisted with 0600 and refresh marker touched.
	loadedTok, err := readTokenFile(tokenPath)
	if err != nil {
		t.Fatalf("readTokenFile: %v", err)
	}
	if loadedTok.AccessToken != "tok-access-1" || loadedTok.RefreshToken != "tok-refresh-1" {
		t.Errorf("loadedTok = %+v", loadedTok)
	}
	if _, err := os.Stat(refreshMarkerPath); err != nil {
		t.Errorf("expected refresh marker file to exist: %v", err)
	}

	// Step 3: When Google omits refresh_token, refresh marker must NOT be created/touched.
	if err := os.Remove(refreshMarkerPath); err != nil {
		t.Fatalf("remove marker: %v", err)
	}
	returnRefresh.Store(false)

	rrRedir2 := httptest.NewRecorder()
	reqRedir2 := httptest.NewRequest(http.MethodGet, "/newtoken", nil)
	svc.RedirectForToken(rrRedir2, reqRedir2)
	u2, _ := url.Parse(rrRedir2.Header().Get("Location"))
	state2 := u2.Query().Get("state")
	var c2 *http.Cookie
	for _, c := range rrRedir2.Result().Cookies() {
		if c.Name == oauthStateCookieName {
			c2 = c
		}
	}

	rrCB2 := httptest.NewRecorder()
	reqCB2 := httptest.NewRequest(http.MethodGet, "/savetoken?state="+state2+"&code=auth-code-2", nil)
	reqCB2.AddCookie(c2)
	svc.ProcessAuthCode(rrCB2, reqCB2)
	if rrCB2.Code != http.StatusSeeOther {
		t.Fatalf("second ProcessAuthCode status = %d, want 303", rrCB2.Code)
	}
	if _, err := os.Stat(refreshMarkerPath); !os.IsNotExist(err) {
		t.Errorf("expected refresh marker NOT to be touched when refresh_token is omitted")
	}

	// Verify live tokSrc keeps the preserved refresh token in memory.
	svc.mu.RLock()
	liveTokSrc := svc.tokSrc
	svc.mu.RUnlock()
	if liveTokSrc == nil {
		t.Fatal("expected live tokSrc after second ProcessAuthCode")
	}
	memTok, err := liveTokSrc.Token()
	if err != nil {
		t.Fatalf("liveTokSrc.Token(): %v", err)
	}
	if memTok.RefreshToken != "tok-refresh-1" {
		t.Errorf("live tokSrc RefreshToken = %q, want tok-refresh-1", memTok.RefreshToken)
	}

	// Step 4: OAuth error query parameter returns 400 Bad Request.
	rrRedir3 := httptest.NewRecorder()
	reqRedir3 := httptest.NewRequest(http.MethodGet, "/newtoken", nil)
	svc.RedirectForToken(rrRedir3, reqRedir3)
	u3, _ := url.Parse(rrRedir3.Header().Get("Location"))
	state3 := u3.Query().Get("state")
	var c3 *http.Cookie
	for _, c := range rrRedir3.Result().Cookies() {
		if c.Name == oauthStateCookieName {
			c3 = c
		}
	}

	rrErr := httptest.NewRecorder()
	reqErr := httptest.NewRequest(http.MethodGet, "/savetoken?state="+state3+"&error=access_denied&error_description=user+denied", nil)
	reqErr.AddCookie(c3)
	svc.ProcessAuthCode(rrErr, reqErr)
	if rrErr.Code != http.StatusBadRequest || !strings.Contains(rrErr.Body.String(), "Authorization failed") {
		t.Errorf("OAuth error response status=%d body=%q, want 400 Authorization failed", rrErr.Code, rrErr.Body.String())
	}
}

func TestSetupServiceAndRefreshTokenLoop(t *testing.T) {
	var tokenCalls atomic.Int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := tokenCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  fmt.Sprintf("refreshed-access-%d", n),
			"refresh_token": "persist-refresh",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	defer tokenSrv.Close()

	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token.json")
	// Write an already-expired token so setupService must refresh it on startup.
	expiredTok := &oauth2.Token{
		AccessToken:  "old-access",
		RefreshToken: "persist-refresh",
		Expiry:       time.Now().Add(-time.Hour),
	}
	if _, err := saveTokenAtomic(tokenPath, expiredTok); err != nil {
		t.Fatalf("save initial token: %v", err)
	}

	svc := &Service{
		loc:             time.UTC,
		tokenFile:       tokenPath,
		nowFn:           time.Now,
		refreshInterval: 15 * time.Millisecond,
		pendingStates:   make(map[string]pendingOAuthState),
		config: &oauth2.Config{
			ClientID:     "cid",
			ClientSecret: "csec",
			Endpoint: oauth2.Endpoint{
				AuthURL:  tokenSrv.URL + "/auth",
				TokenURL: tokenSrv.URL + "/token",
			},
		},
	}
	defer svc.Close()

	if err := svc.setupService(context.Background()); err != nil {
		t.Fatalf("setupService failed: %v", err)
	}
	if svc.TokenExpiry() == "" {
		t.Error("expected TokenExpiry to be populated after setupService")
	}

	loaded, err := readTokenFile(tokenPath)
	if err != nil {
		t.Fatalf("readTokenFile after setupService: %v", err)
	}
	if loaded.AccessToken != "refreshed-access-1" || loaded.RefreshToken != "persist-refresh" {
		t.Errorf("after setupService token = %+v", loaded)
	}

	// Verify refreshTokenLoop persists a newly refreshed token and updates TokenExpiry.
	svc.Close()
	nextExp := time.Date(2026, 7, 1, 15, 0, 0, 0, time.UTC)
	loopTokSrc := oauth2.StaticTokenSource(&oauth2.Token{
		AccessToken: "loop-access-2",
		Expiry:      nextExp,
	})
	loopCtx, loopCancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	svc.mu.Lock()
	svc.tokSrc = loopTokSrc
	svc.accessTok = "refreshed-access-1"
	svc.refreshCancel = loopCancel
	svc.refreshDone = loopDone
	svc.mu.Unlock()

	go func() {
		defer close(loopDone)
		svc.refreshTokenLoop(loopCtx, loopTokSrc)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if svc.TokenExpiry() == nextExp.Format(time.RFC3339) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	svc.Close()

	if got := svc.TokenExpiry(); got != nextExp.Format(time.RFC3339) {
		t.Errorf("TokenExpiry after loop = %q, want %q", got, nextExp.Format(time.RFC3339))
	}
	loadedLoop, err := readTokenFile(tokenPath)
	if err != nil {
		t.Fatalf("readTokenFile after loop: %v", err)
	}
	if loadedLoop.AccessToken != "loop-access-2" || loadedLoop.RefreshToken != "persist-refresh" {
		t.Errorf("after loop token = %+v, want loop-access-2 and persist-refresh", loadedLoop)
	}
}

func TestSaveTokenAtomicAndTouchEmptyPathsAndRefreshPreservation(t *testing.T) {
	if _, err := saveTokenAtomic("", &oauth2.Token{AccessToken: "x"}); err == nil {
		t.Error("expected error for empty token path")
	}
	if _, err := saveTokenAtomic("token.json", nil); err == nil {
		t.Error("expected error for nil token")
	}
	if err := touchFileAtomic("", time.Now()); err == nil {
		t.Error("expected error for empty marker path")
	}

	dir := t.TempDir()
	tokPath := filepath.Join(dir, "token.json")

	initial := &oauth2.Token{
		AccessToken:  "access-1",
		RefreshToken: "refresh-keep",
	}
	savedInitial, err := saveTokenAtomic(tokPath, initial)
	if err != nil {
		t.Fatalf("saveTokenAtomic initial failed: %v", err)
	}
	if savedInitial.RefreshToken != "refresh-keep" {
		t.Errorf("savedInitial.RefreshToken = %q, want refresh-keep", savedInitial.RefreshToken)
	}

	info, err := os.Stat(tokPath)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("token file mode = %o, want 0600", perm)
	}

	refreshed := &oauth2.Token{AccessToken: "access-2"}
	savedRefreshed, err := saveTokenAtomic(tokPath, refreshed)
	if err != nil {
		t.Fatalf("saveTokenAtomic refreshed failed: %v", err)
	}
	if savedRefreshed.AccessToken != "access-2" || savedRefreshed.RefreshToken != "refresh-keep" {
		t.Errorf("savedRefreshed = %+v, want access-2 and refresh-keep", savedRefreshed)
	}

	loaded, err := readTokenFile(tokPath)
	if err != nil {
		t.Fatalf("readTokenFile failed: %v", err)
	}
	if loaded.AccessToken != "access-2" || loaded.RefreshToken != "refresh-keep" {
		t.Errorf("loaded token = %+v, want access-2 and refresh-keep", loaded)
	}
}

func TestProcessAuthCodePreservesRefreshTokenInLiveTokenSource(t *testing.T) {
	var seenRefreshToken atomic.Value
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.FormValue("grant_type") == "refresh_token" {
			seenRefreshToken.Store(r.FormValue("refresh_token"))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "refreshed-live-access",
				"token_type":   "Bearer",
				"expires_in":   3600,
			})
			return
		}
		// Authorization code exchange omits refresh_token.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "reauth-access-no-refresh",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	}))
	defer tokenSrv.Close()

	dir := t.TempDir()
	clientSecretPath := filepath.Join(dir, "client_secret.json")
	tokenPath := filepath.Join(dir, "token.json")
	refreshMarkerPath := filepath.Join(dir, "refresh_tok_obtained.empty")

	clientSecretJSON := fmt.Sprintf(`{"web":{"client_id":"cid","client_secret":"csec","auth_uri":"%s/auth","token_uri":"%s/token","redirect_uris":["https://example.com/savetoken"]}}`, tokenSrv.URL, tokenSrv.URL)
	if err := os.WriteFile(clientSecretPath, []byte(clientSecretJSON), 0600); err != nil {
		t.Fatalf("write client_secret: %v", err)
	}

	seedTok := &oauth2.Token{
		AccessToken:  "seed-access",
		RefreshToken: "preserved-refresh-token",
		Expiry:       time.Now().Add(time.Hour),
	}
	if _, err := saveTokenAtomic(tokenPath, seedTok); err != nil {
		t.Fatalf("seed token.json: %v", err)
	}

	svc, err := NewService(context.Background(), Options{
		Location:         time.UTC,
		TokenFile:        tokenPath,
		RefreshTokenFile: refreshMarkerPath,
		ClientSecretFile: clientSecretPath,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	defer svc.Close()

	rrRedir := httptest.NewRecorder()
	svc.RedirectForToken(rrRedir, httptest.NewRequest(http.MethodGet, "/newtoken", nil))
	u, err := url.Parse(rrRedir.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	state := u.Query().Get("state")
	var stateCookie *http.Cookie
	for _, c := range rrRedir.Result().Cookies() {
		if c.Name == oauthStateCookieName {
			stateCookie = c
		}
	}

	rrCB := httptest.NewRecorder()
	reqCB := httptest.NewRequest(http.MethodGet, "/savetoken?state="+state+"&code=reauth-code", nil)
	reqCB.AddCookie(stateCookie)
	svc.ProcessAuthCode(rrCB, reqCB)
	if rrCB.Code != http.StatusSeeOther {
		t.Fatalf("ProcessAuthCode status = %d, want 303", rrCB.Code)
	}

	svc.mu.RLock()
	tokSrc := svc.tokSrc
	cfg := svc.config
	svc.mu.RUnlock()
	if tokSrc == nil {
		t.Fatal("expected live tokSrc after ProcessAuthCode")
	}

	liveTok, err := tokSrc.Token()
	if err != nil {
		t.Fatalf("tokSrc.Token(): %v", err)
	}
	if liveTok.AccessToken != "reauth-access-no-refresh" {
		t.Errorf("liveTok.AccessToken = %q, want reauth-access-no-refresh", liveTok.AccessToken)
	}
	if liveTok.RefreshToken != "preserved-refresh-token" {
		t.Fatalf("liveTok.RefreshToken = %q, want preserved-refresh-token", liveTok.RefreshToken)
	}

	// Confirm the preserved refresh token succeeds when the access token expires.
	expiredCopy := *liveTok
	expiredCopy.Expiry = time.Now().Add(-time.Minute)
	refreshedTok, err := cfg.TokenSource(context.Background(), &expiredCopy).Token()
	if err != nil {
		t.Fatalf("refresh with preserved token failed: %v", err)
	}
	if refreshedTok.AccessToken != "refreshed-live-access" {
		t.Errorf("refreshedTok.AccessToken = %q, want refreshed-live-access", refreshedTok.AccessToken)
	}
	if got, _ := seenRefreshToken.Load().(string); got != "preserved-refresh-token" {
		t.Errorf("token server saw refresh_token = %q, want preserved-refresh-token", got)
	}
}

func TestAllDayEventTimezoneBoundaryFiltering(t *testing.T) {
	zurich, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		t.Fatalf("load Europe/Zurich: %v", err)
	}
	target := time.Date(2026, 6, 15, 10, 0, 0, 0, zurich)

	fetcher := stubFetcher{
		fn: func(_, _, _ string) ([]*gcal.Event, error) {
			return []*gcal.Event{
				// Yesterday's UTC all-day event (ends 2026-06-15 00:00Z > 2026-06-14 22:00Z).
				{Id: "yesterday", Summary: "Yesterday Holiday", Start: &gcal.EventDateTime{Date: "2026-06-14"}, End: &gcal.EventDateTime{Date: "2026-06-15"}},
				// Tomorrow's UTC all-day event (could leak in negative UTC offsets).
				{Id: "tomorrow", Summary: "Tomorrow Holiday", Start: &gcal.EventDateTime{Date: "2026-06-16"}, End: &gcal.EventDateTime{Date: "2026-06-17"}},
				// Multi-day all-day event covering today.
				{Id: "multiday", Summary: "Conference", Start: &gcal.EventDateTime{Date: "2026-06-14"}, End: &gcal.EventDateTime{Date: "2026-06-16"}},
				// Single-day all-day event for today with exclusive end.
				{Id: "today", Summary: "Today Holiday", Start: &gcal.EventDateTime{Date: "2026-06-15"}, End: &gcal.EventDateTime{Date: "2026-06-16"}},
				// Single-day all-day event with omitted End.
				{Id: "today-no-end", Summary: "Today No End", Start: &gcal.EventDateTime{Date: "2026-06-15"}},
			}, nil
		},
	}

	svc, err := NewService(context.Background(), Options{
		Calendars: []string{"primary"},
		Location:  zurich,
		Now:       func() time.Time { return target },
		Fetcher:   fetcher,
	})
	if err != nil {
		t.Fatalf("NewService failed: %v", err)
	}

	events, err := svc.Events(context.Background(), target)
	if err != nil {
		t.Fatalf("Events failed: %v", err)
	}
	gotIDs := make(map[string]bool, len(events))
	for _, ev := range events {
		gotIDs[ev.Id] = true
	}
	if gotIDs["yesterday"] {
		t.Error("yesterday all-day event leaked onto today")
	}
	if gotIDs["tomorrow"] {
		t.Error("tomorrow all-day event leaked onto today")
	}
	for _, want := range []string{"multiday", "today", "today-no-end"} {
		if !gotIDs[want] {
			t.Errorf("expected all-day event %q on target date", want)
		}
	}
}

type panicTokenSource struct{}

func (panicTokenSource) Token() (*oauth2.Token, error) {
	panic("TokenExpiry must not call tokSrc.Token()")
}

func TestTokenExpiryUsesCachedValueWithoutNetwork(t *testing.T) {
	zurich, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		t.Fatalf("load Europe/Zurich: %v", err)
	}
	exp := time.Date(2026, 6, 15, 11, 30, 0, 0, time.UTC)

	svc, err := NewService(context.Background(), Options{
		Location: zurich,
		Fetcher:  stubFetcher{fn: func(_, _, _ string) ([]*gcal.Event, error) { return nil, nil }},
	})
	if err != nil {
		t.Fatalf("NewService failed: %v", err)
	}

	svc.mu.Lock()
	svc.tokSrc = panicTokenSource{}
	svc.tokenExpiry = exp
	svc.mu.Unlock()

	got := svc.TokenExpiry()
	want := exp.In(zurich).Format(time.RFC3339)
	if got != want {
		t.Errorf("TokenExpiry() = %q, want %q", got, want)
	}
}

func TestCloseWaitsForRefreshLoopExit(t *testing.T) {
	svc, err := NewService(context.Background(), Options{
		Location: time.UTC,
		Fetcher:  stubFetcher{fn: func(_, _, _ string) ([]*gcal.Event, error) { return nil, nil }},
	})
	if err != nil {
		t.Fatalf("NewService failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	svc.mu.Lock()
	svc.refreshCancel = cancel
	svc.refreshDone = done
	svc.mu.Unlock()

	go func() {
		defer close(done)
		svc.refreshTokenLoop(ctx, panicTokenSource{})
	}()

	svc.Close()
	select {
	case <-done:
	default:
		t.Error("expected Close() to wait for refreshTokenLoop exit")
	}
}

func TestDeterministicDeduplicationAndBoundedStates(t *testing.T) {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	fetcher := stubFetcher{
		fn: func(calID, _, _ string) ([]*gcal.Event, error) {
			if calID == "first@example.com" {
				time.Sleep(15 * time.Millisecond) // Finish second to verify config order still wins.
				return []*gcal.Event{
					{Id: "shared", Summary: "Full Title", Start: &gcal.EventDateTime{Date: "2026-06-15"}},
				}, nil
			}
			return []*gcal.Event{
				{Id: "shared", Summary: "Busy", Start: &gcal.EventDateTime{Date: "2026-06-15"}},
			}, nil
		},
	}

	svc, err := NewService(context.Background(), Options{
		Calendars: []string{"first@example.com", "second@example.com"},
		Location:  time.UTC,
		Now:       func() time.Time { return now },
		Fetcher:   fetcher,
	})
	if err != nil {
		t.Fatalf("NewService failed: %v", err)
	}

	events, err := svc.Events(context.Background(), now)
	if err != nil {
		t.Fatalf("Events failed: %v", err)
	}
	if len(events) != 1 || events[0].Summary != "Full Title" {
		t.Errorf("dedup result = %+v, want first calendar's Full Title", events)
	}

	for i := 0; i < maxPendingStates+15; i++ {
		if _, err := svc.issueState(); err != nil {
			t.Fatalf("issueState: %v", err)
		}
	}
	svc.stateMu.Lock()
	gotStates := len(svc.pendingStates)
	svc.stateMu.Unlock()
	if gotStates > maxPendingStates {
		t.Errorf("pendingStates size = %d, want <= %d", gotStates, maxPendingStates)
	}

	// Missing client_secret.json preserves startup error in Events().
	missingSvc, err := NewService(context.Background(), Options{
		Location:         time.UTC,
		ClientSecretFile: filepath.Join(t.TempDir(), "missing.json"),
	})
	if err == nil {
		t.Fatal("expected init error for missing client_secret.json")
	}
	if _, evErr := missingSvc.Events(context.Background(), now); evErr == nil || !strings.Contains(evErr.Error(), "read client secret file") {
		t.Errorf("Events() error = %v, want preserved startup error", evErr)
	}
}
