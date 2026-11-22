package calendar

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	gcal "google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"

	"github.com/neoautomata/brd/internal/atomicfile"
)

const (
	oauthStateBytes = 32
	oauthStateTTL   = 10 * time.Minute
	fetchTimeout    = 15 * time.Second
)

// EventFetcher lists calendar events for one calendar ID in a time range.
type EventFetcher interface {
	ListEvents(ctx context.Context, calID, timeMin, timeMax string) ([]*gcal.Event, error)
}

type googleEventFetcher struct {
	srv *gcal.Service
}

func (g *googleEventFetcher) ListEvents(ctx context.Context, calID, timeMin, timeMax string) ([]*gcal.Event, error) {
	resp, err := g.srv.Events.List(calID).
		Context(ctx).
		ShowDeleted(false).
		SingleEvents(true).
		TimeMin(timeMin).
		TimeMax(timeMax).
		OrderBy("startTime").
		Do()
	if err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// Options configures a calendar Service.
type Options struct {
	Calendars        []string
	Location         *time.Location
	TokenFile        string
	RefreshTokenFile string
	ClientSecretFile string
	Now              func() time.Time
	Fetcher          EventFetcher
}

// Service manages Google Calendar OAuth2 state and renders daily event HTML.
type Service struct {
	calendars        []string
	loc              *time.Location
	tokenFile        string
	refreshTokenFile string
	clientSecretFile string
	nowFn            func() time.Time

	mu            sync.RWMutex
	config        *oauth2.Config
	fetcher       EventFetcher
	srvErr        error
	tokSrc        oauth2.TokenSource
	accessTok     string
	refreshCancel context.CancelFunc

	stateMu       sync.Mutex
	pendingStates map[string]time.Time
}

// NewService creates a calendar Service and attempts initial OAuth setup when no custom Fetcher is provided.
func NewService(ctx context.Context, opts Options) (*Service, error) {
	loc := opts.Location
	if loc == nil {
		loc = time.UTC
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	cals := make([]string, len(opts.Calendars))
	copy(cals, opts.Calendars)

	s := &Service{
		calendars:        cals,
		loc:              loc,
		tokenFile:        opts.TokenFile,
		refreshTokenFile: opts.RefreshTokenFile,
		clientSecretFile: opts.ClientSecretFile,
		nowFn:            nowFn,
		fetcher:          opts.Fetcher,
		pendingStates:    make(map[string]time.Time),
	}

	if s.fetcher != nil {
		return s, nil
	}
	return s, s.init(ctx)
}

// Close stops the background OAuth token refresh loop.
func (s *Service) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	cancel := s.refreshCancel
	s.refreshCancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// TokenExpiry returns the current OAuth access token expiry formatted as RFC3339, or empty if unavailable.
func (s *Service) TokenExpiry() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	tokSrc := s.tokSrc
	s.mu.RUnlock()
	if tokSrc == nil {
		return ""
	}
	tok, err := tokSrc.Token()
	if err != nil {
		return ""
	}
	return tok.Expiry.In(s.loc).Format(time.RFC3339)
}

func (s *Service) init(ctx context.Context) error {
	if s.clientSecretFile == "" {
		return errors.New("client secret path is empty")
	}
	b, err := os.ReadFile(s.clientSecretFile)
	if err != nil {
		return fmt.Errorf("read client secret file: %w", err)
	}

	cfg, err := google.ConfigFromJSON(b, gcal.CalendarReadonlyScope)
	if err != nil {
		return fmt.Errorf("parse client secret JSON: %w", err)
	}

	s.mu.Lock()
	s.config = cfg
	s.mu.Unlock()

	if err := s.setupService(ctx); err != nil {
		return fmt.Errorf("setup calendar service: %w", err)
	}
	return nil
}

func (s *Service) setupService(ctx context.Context) error {
	s.mu.RLock()
	cfg := s.config
	s.mu.RUnlock()

	if cfg == nil {
		err := errors.New("oauth2 config is nil")
		s.mu.Lock()
		s.fetcher = nil
		s.tokSrc = nil
		s.accessTok = ""
		s.srvErr = err
		s.mu.Unlock()
		return err
	}

	tok, err := readTokenFile(s.tokenFile)
	if err != nil {
		wrapErr := fmt.Errorf("invalid token file, visit /newtoken: %w", err)
		s.recordSetupError(wrapErr)
		return wrapErr
	}

	initCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	// Refresh and verify the token outside s.mu with a bounded timeout context.
	initTokSrc := cfg.TokenSource(initCtx, tok)
	liveTok, err := initTokSrc.Token()
	if err != nil {
		wrapErr := fmt.Errorf("obtain oauth token: %w", err)
		s.recordSetupError(wrapErr)
		return wrapErr
	}
	if !liveTok.Valid() {
		expErr := errors.New("token expired and failed renewal, visit /newtoken")
		s.recordSetupError(expErr)
		return expErr
	}

	if liveTok.AccessToken != tok.AccessToken {
		if err := saveTokenAtomic(s.tokenFile, liveTok); err != nil {
			log.Printf("Failed to save renewed token on startup: %v", err)
		}
	}

	bgCtx := context.Background()
	tokSrc := cfg.TokenSource(bgCtx, liveTok)
	client := oauth2.NewClient(bgCtx, tokSrc)

	srv, err := gcal.NewService(initCtx, option.WithHTTPClient(client))
	if err != nil {
		s.recordSetupError(err)
		return err
	}

	refreshCtx, refreshCancel := context.WithCancel(bgCtx)

	s.mu.Lock()
	if s.refreshCancel != nil {
		s.refreshCancel()
	}
	s.fetcher = &googleEventFetcher{srv: srv}
	s.tokSrc = tokSrc
	s.accessTok = liveTok.AccessToken
	s.srvErr = nil
	s.refreshCancel = refreshCancel
	s.mu.Unlock()

	go s.refreshTokenLoop(refreshCtx, tokSrc)
	return nil
}

func (s *Service) recordSetupError(err error) {
	s.mu.Lock()
	if s.refreshCancel != nil {
		s.refreshCancel()
		s.refreshCancel = nil
	}
	s.fetcher = nil
	s.tokSrc = nil
	s.accessTok = ""
	s.srvErr = err
	s.mu.Unlock()
}

func (s *Service) refreshTokenLoop(ctx context.Context, tokSrc oauth2.TokenSource) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tok, err := tokSrc.Token()
			if err != nil {
				log.Printf("Refresh token failed: %v", err)
				continue
			}
			s.mu.Lock()
			if s.accessTok != tok.AccessToken {
				if err := saveTokenAtomic(s.tokenFile, tok); err != nil {
					log.Printf("Save refreshed token failed: %v", err)
					s.mu.Unlock()
					continue
				}
				s.accessTok = tok.AccessToken
				log.Print("Saved refreshed OAuth token")
			}
			s.mu.Unlock()
		}
	}
}

// Events fetches, deduplicates, and sorts events for the day containing t.
func (s *Service) Events(ctx context.Context, t time.Time) ([]*gcal.Event, error) {
	s.mu.RLock()
	fetcher := s.fetcher
	srvErr := s.srvErr
	s.mu.RUnlock()

	if fetcher == nil {
		if srvErr != nil {
			return nil, srvErr
		}
		return nil, errors.New("calendar service is not initialized")
	}
	if len(s.calendars) == 0 {
		return nil, nil
	}

	localDay := t.In(s.loc)
	midnight := time.Date(localDay.Year(), localDay.Month(), localDay.Day(), 0, 0, 0, 0, s.loc)
	timeMin := midnight.Format(time.RFC3339)
	timeMax := midnight.AddDate(0, 0, 1).Format(time.RFC3339)

	type calResult struct {
		cal   string
		items []*gcal.Event
		err   error
	}

	fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	resChan := make(chan calResult, len(s.calendars))
	var wg sync.WaitGroup
	for _, calID := range s.calendars {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			items, err := fetcher.ListEvents(fetchCtx, id, timeMin, timeMax)
			resChan <- calResult{cal: id, items: items, err: err}
		}(calID)
	}
	wg.Wait()
	close(resChan)

	var events []*gcal.Event
	var firstErr error
	successCount := 0
	for res := range resChan {
		if res.err != nil {
			log.Printf("Calendar fetch failed for %s: %v", res.cal, res.err)
			if firstErr == nil {
				firstErr = res.err
			}
			continue
		}
		successCount++
		events = append(events, res.items...)
	}

	if successCount == 0 && firstErr != nil {
		return nil, fmt.Errorf("all calendar requests failed: %w", firstErr)
	}

	// Deduplicate by event ID while dropping nil entries.
	seen := make(map[string]struct{}, len(events))
	deduped := events[:0]
	for _, ev := range events {
		if ev == nil {
			continue
		}
		if ev.Id != "" {
			if _, ok := seen[ev.Id]; ok {
				continue
			}
			seen[ev.Id] = struct{}{}
		}
		deduped = append(deduped, ev)
	}

	sortEvents(deduped)
	return deduped, nil
}

// RenderHTML returns an HTML unordered list of upcoming events for t.
// When fewer than four events remain on t, it appends tomorrow's events.
func (s *Service) RenderHTML(ctx context.Context, t time.Time) (string, error) {
	if s == nil {
		return "", errors.New("calendar service is not initialized")
	}

	events, err := s.Events(ctx, t)
	if err != nil {
		return "", fmt.Errorf("retrieve calendar events: %w", err)
	}

	now := s.nowFn()
	var lines []string
	for _, ev := range events {
		if li := formatEventLi(ev, now, s.loc); li != "" {
			lines = append(lines, li)
		}
	}

	if len(lines) < 4 {
		tomorrowEvents, err := s.Events(ctx, t.AddDate(0, 0, 1))
		if err != nil {
			log.Printf("Retrieve tomorrow calendar events failed: %v", err)
		} else {
			var tomorrowLines []string
			for _, ev := range tomorrowEvents {
				if li := formatEventLi(ev, now, s.loc); li != "" {
					tomorrowLines = append(tomorrowLines, li)
				}
			}
			if len(tomorrowLines) > 0 {
				spacing := ""
				if len(lines) > 0 {
					spacing = ` style="padding-top: 24px;"`
				}
				lines = append(lines, "<li"+spacing+"><b>Tomorrow ...</b></li>")
				lines = append(lines, tomorrowLines...)
			}
		}
	}

	if len(lines) == 0 {
		return "", nil
	}
	return fmt.Sprintf("<ul style=\"font-size:32px;margin-top:12px;margin-bottom:12px;\">\n%s\n</ul>\n", strings.Join(lines, "\n")), nil
}

// RedirectForToken starts the OAuth2 authorization flow with a random state token.
func (s *Service) RedirectForToken(w http.ResponseWriter, r *http.Request) {
	if s == nil {
		http.Error(w, "Calendar service unavailable", http.StatusInternalServerError)
		return
	}

	s.mu.RLock()
	cfg := s.config
	s.mu.RUnlock()

	if cfg == nil {
		if err := s.init(r.Context()); err != nil {
			log.Printf("Calendar init failed during OAuth redirect: %v", err)
		}
		s.mu.RLock()
		cfg = s.config
		s.mu.RUnlock()
		if cfg == nil {
			http.Error(w, "Calendar configuration unavailable", http.StatusInternalServerError)
			return
		}
	}

	state, err := s.issueState()
	if err != nil {
		log.Printf("Generate OAuth state failed: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	authURL := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)
	http.Redirect(w, r, authURL, http.StatusSeeOther)
}

// ProcessAuthCode validates the OAuth state token, exchanges the code, and persists the new token.
func (s *Service) ProcessAuthCode(w http.ResponseWriter, r *http.Request) {
	if s == nil {
		http.Error(w, "Calendar service unavailable", http.StatusInternalServerError)
		return
	}

	s.mu.RLock()
	cfg := s.config
	s.mu.RUnlock()
	if cfg == nil {
		http.Error(w, "Calendar configuration unavailable", http.StatusInternalServerError)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	state := r.FormValue("state")
	if !s.consumeState(state) {
		http.Error(w, "Invalid or expired OAuth state", http.StatusBadRequest)
		return
	}

	code := r.FormValue("code")
	if code == "" {
		http.Error(w, "Missing authorization code", http.StatusBadRequest)
		return
	}

	tok, err := cfg.Exchange(r.Context(), code)
	if err != nil {
		log.Printf("OAuth code exchange failed: %v", err)
		http.Error(w, "Token exchange failed", http.StatusInternalServerError)
		return
	}

	if err := saveTokenAtomic(s.tokenFile, tok); err != nil {
		log.Printf("Save OAuth token failed: %v", err)
		http.Error(w, "Failed to save token", http.StatusInternalServerError)
		return
	}

	if err := touchFileAtomic(s.refreshTokenFile, s.nowFn()); err != nil {
		log.Printf("Record refresh timestamp failed: %v", err)
	}

	if err := s.setupService(r.Context()); err != nil {
		log.Printf("Calendar service setup failed: %v", err)
		http.Error(w, "Failed to initialize calendar service", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Service) issueState() (string, error) {
	buf := make([]byte, oauthStateBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	now := s.nowFn()

	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.pruneStatesLocked(now)
	s.pendingStates[token] = now.Add(oauthStateTTL)
	return token, nil
}

func (s *Service) consumeState(candidate string) bool {
	if candidate == "" {
		return false
	}
	now := s.nowFn()

	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.pruneStatesLocked(now)

	matchedKey := ""
	for stored := range s.pendingStates {
		if subtle.ConstantTimeCompare([]byte(stored), []byte(candidate)) == 1 {
			matchedKey = stored
		}
	}
	if matchedKey == "" {
		return false
	}
	delete(s.pendingStates, matchedKey)
	return true
}

func (s *Service) pruneStatesLocked(now time.Time) {
	for k, exp := range s.pendingStates {
		if !exp.After(now) {
			delete(s.pendingStates, k)
		}
	}
}

func sortEvents(events []*gcal.Event) {
	sort.Slice(events, func(i, j int) bool {
		a, b := events[i], events[j]

		// Strict weak ordering for nil Start values: non-nil sorts before nil.
		if (a.Start == nil) != (b.Start == nil) {
			return a.Start != nil
		}
		if a.Start == nil && b.Start == nil {
			if a.Summary != b.Summary {
				return a.Summary < b.Summary
			}
			return a.Id < b.Id
		}

		aAllDay := a.Start.Date != ""
		bAllDay := b.Start.Date != ""
		if aAllDay != bAllDay {
			return aAllDay
		}
		if aAllDay && bAllDay {
			if a.Summary != b.Summary {
				return a.Summary < b.Summary
			}
			return a.Id < b.Id
		}

		aStart, aStartErr := time.Parse(time.RFC3339, a.Start.DateTime)
		bStart, bStartErr := time.Parse(time.RFC3339, b.Start.DateTime)
		if (aStartErr == nil) != (bStartErr == nil) {
			return aStartErr == nil
		}
		if aStartErr == nil && !aStart.Equal(bStart) {
			return aStart.Before(bStart)
		}

		aHasEnd := a.End != nil && a.End.DateTime != ""
		bHasEnd := b.End != nil && b.End.DateTime != ""
		if aHasEnd != bHasEnd {
			return aHasEnd
		}
		if aHasEnd && bHasEnd {
			aEnd, aEndErr := time.Parse(time.RFC3339, a.End.DateTime)
			bEnd, bEndErr := time.Parse(time.RFC3339, b.End.DateTime)
			if (aEndErr == nil) != (bEndErr == nil) {
				return aEndErr == nil
			}
			if aEndErr == nil && !aEnd.Equal(bEnd) {
				return aEnd.Before(bEnd)
			}
		}

		if a.Summary != b.Summary {
			return a.Summary < b.Summary
		}
		return a.Id < b.Id
	})
}

func formatEventLi(event *gcal.Event, now time.Time, loc *time.Location) string {
	if event == nil || event.Start == nil {
		return ""
	}
	escapedSummary := html.EscapeString(event.Summary)
	if event.Start.Date != "" {
		return fmt.Sprintf("<li>%s</li>", escapedSummary)
	}

	start, err := time.Parse(time.RFC3339, event.Start.DateTime)
	if err != nil {
		log.Printf("Skip event with invalid start timestamp %q: %v", event.Start.DateTime, err)
		return ""
	}
	if event.End == nil || event.End.DateTime == "" {
		return fmt.Sprintf("<li>%s: %s</li>", start.In(loc).Format("15:04"), escapedSummary)
	}

	end, err := time.Parse(time.RFC3339, event.End.DateTime)
	if err != nil {
		log.Printf("Skip event with invalid end timestamp %q: %v", event.End.DateTime, err)
		return ""
	}
	if end.Before(now) {
		return ""
	}
	return fmt.Sprintf("<li>%s - %s: %s</li>", start.In(loc).Format("15:04"), end.In(loc).Format("15:04"), escapedSummary)
}

func readTokenFile(path string) (*oauth2.Token, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	if err := json.NewDecoder(f).Decode(tok); err != nil {
		return nil, err
	}
	return tok, nil
}

func saveTokenAtomic(path string, token *oauth2.Token) error {
	tokCopy := *token
	if tokCopy.RefreshToken == "" {
		if existing, err := readTokenFile(path); err == nil && existing.RefreshToken != "" {
			tokCopy.RefreshToken = existing.RefreshToken
		}
	}

	data, err := json.Marshal(&tokCopy)
	if err != nil {
		return fmt.Errorf("marshal oauth token: %w", err)
	}
	return atomicfile.Write(path, data, 0600)
}

func touchFileAtomic(path string, ts time.Time) error {
	if err := atomicfile.Write(path, nil, 0600); err != nil {
		return err
	}
	return os.Chtimes(path, ts, ts)
}
