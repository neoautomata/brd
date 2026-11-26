package calendar

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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

	// Wrong state rejected
	rrBad := httptest.NewRecorder()
	reqBad := httptest.NewRequest(http.MethodGet, "/savetoken?state=wrong&code=abc", nil)
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

func TestSaveTokenAtomicPermissionsAndRefreshPreservation(t *testing.T) {
	dir := t.TempDir()
	tokPath := filepath.Join(dir, "token.json")

	initial := &oauth2.Token{
		AccessToken:  "access-1",
		RefreshToken: "refresh-keep",
	}
	if err := saveTokenAtomic(tokPath, initial); err != nil {
		t.Fatalf("saveTokenAtomic initial failed: %v", err)
	}

	info, err := os.Stat(tokPath)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("token file mode = %o, want 0600", perm)
	}

	renewed := &oauth2.Token{AccessToken: "access-2"}
	if err := saveTokenAtomic(tokPath, renewed); err != nil {
		t.Fatalf("saveTokenAtomic renewed failed: %v", err)
	}

	loaded, err := readTokenFile(tokPath)
	if err != nil {
		t.Fatalf("readTokenFile failed: %v", err)
	}
	if loaded.AccessToken != "access-2" || loaded.RefreshToken != "refresh-keep" {
		t.Errorf("loaded token = %+v, want access-2 and refresh-keep", loaded)
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
