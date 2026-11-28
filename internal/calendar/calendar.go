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
	oauthStateBytes        = 32
	oauthStateTTL          = 10 * time.Minute
	maxPendingStates       = 256
	oauthStateCookieName   = "brd_oauth_state"
	fetchTimeout           = 15 * time.Second
	tomorrowFetchTimeout   = 5 * time.Second
	defaultRefreshInterval = 5 * time.Minute
	maxEventPages          = 20
)

type pendingOAuthState struct {
	verifier string
	expires  time.Time
}

// EventFetcher lists calendar events for one calendar ID in a time range.
type EventFetcher interface {
	ListEvents(ctx context.Context, calID, timeMin, timeMax string) ([]*gcal.Event, error)
}

type googleEventFetcher struct {
	srv *gcal.Service
}

func (g *googleEventFetcher) ListEvents(ctx context.Context, calID, timeMin, timeMax string) ([]*gcal.Event, error) {
	var items []*gcal.Event
	pageToken := ""
	for page := 0; page < maxEventPages; page++ {
		call := g.srv.Events.List(calID).
			Context(ctx).
			ShowDeleted(false).
			SingleEvents(true).
			TimeMin(timeMin).
			TimeMax(timeMax).
			OrderBy("startTime")
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		resp, err := call.Do()
		if err != nil {
			return nil, err
		}
		items = append(items, resp.Items...)
		if resp.NextPageToken == "" {
			return items, nil
		}
		pageToken = resp.NextPageToken
	}
	log.Printf("Warning: calendar %s reached page limit %d and truncated results", calID, maxEventPages)
	return items, nil
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

// Service manages Google Calendar OAuth state and renders daily event HTML.
type Service struct {
	calendars        []string
	loc              *time.Location
	tokenFile        string
	refreshTokenFile string
	clientSecretFile string
	nowFn            func() time.Time
	refreshInterval  time.Duration

	mu            sync.RWMutex
	config        *oauth2.Config
	fetcher       EventFetcher
	srvErr        error
	tokSrc        oauth2.TokenSource
	accessTok     string
	tokenExpiry   time.Time
	refreshCancel context.CancelFunc
	refreshDone   chan struct{}

	setupMu     sync.Mutex
	tokenFileMu sync.Mutex

	stateMu       sync.Mutex
	pendingStates map[string]pendingOAuthState
}

// NewService creates a calendar Service and initializes OAuth when opts.Fetcher is nil.
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
		refreshInterval:  defaultRefreshInterval,
		fetcher:          opts.Fetcher,
		pendingStates:    make(map[string]pendingOAuthState),
	}

	if s.fetcher != nil {
		return s, nil
	}
	return s, s.init(ctx)
}

// Close stops the refresh loop for OAuth tokens and waits for it to exit.
func (s *Service) Close() {
	if s == nil {
		return
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	s.stopRefreshLoop()
}

func (s *Service) stopRefreshLoop() {
	s.mu.Lock()
	cancel := s.refreshCancel
	done := s.refreshDone
	s.refreshCancel = nil
	s.refreshDone = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// TokenExpiry returns the expiry of the cached OAuth token in RFC3339 format.
// It returns an empty string when no expiry is available.
func (s *Service) TokenExpiry() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	expiry := s.tokenExpiry
	s.mu.RUnlock()
	if expiry.IsZero() {
		return ""
	}
	return expiry.In(s.loc).Format(time.RFC3339)
}

func oauthHTTPContext(parent context.Context) context.Context {
	return context.WithValue(parent, oauth2.HTTPClient, &http.Client{Timeout: fetchTimeout})
}

func isHTTPSRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Service) loadOAuthConfig() (*oauth2.Config, error) {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	return s.loadOAuthConfigLocked()
}

func (s *Service) loadOAuthConfigLocked() (*oauth2.Config, error) {
	s.mu.RLock()
	cfg := s.config
	s.mu.RUnlock()
	if cfg != nil {
		return cfg, nil
	}

	if strings.TrimSpace(s.clientSecretFile) == "" {
		return nil, errors.New("client secret path is empty")
	}
	warnInsecureFilePerms(s.clientSecretFile)
	b, err := os.ReadFile(s.clientSecretFile)
	if err != nil {
		return nil, fmt.Errorf("read client secret file: %w", err)
	}

	cfg, err = google.ConfigFromJSON(b, gcal.CalendarReadonlyScope)
	if err != nil {
		return nil, fmt.Errorf("parse client secret JSON: %w", err)
	}

	s.mu.Lock()
	s.config = cfg
	s.mu.Unlock()
	return cfg, nil
}

func (s *Service) init(ctx context.Context) error {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()

	if _, err := s.loadOAuthConfigLocked(); err != nil {
		s.recordSetupError(err)
		return err
	}
	if err := s.setupServiceLocked(ctx, nil); err != nil {
		return fmt.Errorf("initialize calendar service: %w", err)
	}
	return nil
}

func (s *Service) setupService(ctx context.Context) error {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	return s.setupServiceLocked(ctx, nil)
}

func (s *Service) setupServiceLocked(ctx context.Context, providedTok *oauth2.Token) error {
	s.mu.RLock()
	cfg := s.config
	s.mu.RUnlock()

	if cfg == nil {
		err := errors.New("oauth config is nil")
		s.recordSetupError(err)
		return err
	}

	tok := providedTok
	if tok == nil {
		s.tokenFileMu.Lock()
		warnInsecureFilePerms(s.tokenFile)
		var err error
		tok, err = readTokenFile(s.tokenFile)
		s.tokenFileMu.Unlock()
		if err != nil {
			wrapErr := fmt.Errorf("invalid token file, visit /newtoken: %w", err)
			s.recordSetupError(wrapErr)
			return wrapErr
		}
	}

	initCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	initOAuthCtx := oauthHTTPContext(initCtx)

	// Refresh and validate the token outside s.mu with a bounded timeout.
	initTokSrc := cfg.TokenSource(initOAuthCtx, tok)
	liveTok, err := initTokSrc.Token()
	if err != nil {
		wrapErr := fmt.Errorf("fetch oauth token: %w", err)
		s.recordSetupError(wrapErr)
		return wrapErr
	}
	if !liveTok.Valid() {
		expErr := errors.New("token expired and failed refresh, visit /newtoken")
		s.recordSetupError(expErr)
		return expErr
	}
	if liveTok.RefreshToken == "" && tok.RefreshToken != "" {
		liveTokCopy := *liveTok
		liveTokCopy.RefreshToken = tok.RefreshToken
		liveTok = &liveTokCopy
	}

	// Stop the prior refresh loop only after the new token validates.
	s.stopRefreshLoop()

	if liveTok.AccessToken != tok.AccessToken || providedTok != nil {
		s.tokenFileMu.Lock()
		savedTok, saveErr := saveTokenAtomic(s.tokenFile, liveTok)
		s.tokenFileMu.Unlock()
		if saveErr != nil {
			log.Printf("Save refreshed token failed: %v", saveErr)
		} else {
			liveTok = savedTok
		}
	}

	refreshCtx, refreshCancel := context.WithCancel(context.Background())
	oauthCtx := oauthHTTPContext(refreshCtx)
	tokSrc := cfg.TokenSource(oauthCtx, liveTok)
	client := oauth2.NewClient(oauthCtx, tokSrc)

	srv, err := gcal.NewService(initCtx, option.WithHTTPClient(client))
	if err != nil {
		refreshCancel()
		s.recordSetupError(err)
		return err
	}

	done := make(chan struct{})
	s.mu.Lock()
	s.fetcher = &googleEventFetcher{srv: srv}
	s.tokSrc = tokSrc
	s.accessTok = liveTok.AccessToken
	s.tokenExpiry = liveTok.Expiry
	s.srvErr = nil
	s.refreshCancel = refreshCancel
	s.refreshDone = done
	s.mu.Unlock()

	go func() {
		defer close(done)
		s.refreshTokenLoop(refreshCtx, tokSrc)
	}()
	return nil
}

func (s *Service) recordSetupError(err error) {
	s.stopRefreshLoop()
	s.mu.Lock()
	s.fetcher = nil
	s.tokSrc = nil
	s.accessTok = ""
	s.tokenExpiry = time.Time{}
	s.srvErr = err
	s.mu.Unlock()
}

func (s *Service) refreshTokenLoop(ctx context.Context, tokSrc oauth2.TokenSource) {
	interval := s.refreshInterval
	if interval <= 0 {
		interval = defaultRefreshInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tok, err := tokSrc.Token()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("Refresh token failed: %v", err)
				continue
			}

			s.mu.RLock()
			stale := ctx.Err() != nil || s.tokSrc != tokSrc
			changed := s.accessTok != tok.AccessToken
			s.mu.RUnlock()
			if stale {
				return
			}

			if changed {
				s.tokenFileMu.Lock()
				s.mu.RLock()
				stillActive := ctx.Err() == nil && s.tokSrc == tokSrc
				s.mu.RUnlock()
				if !stillActive {
					s.tokenFileMu.Unlock()
					return
				}
				_, saveErr := saveTokenAtomic(s.tokenFile, tok)
				s.tokenFileMu.Unlock()
				if saveErr != nil {
					log.Printf("Save refreshed token failed: %v", saveErr)
					continue
				}
			}

			s.mu.Lock()
			if ctx.Err() != nil || s.tokSrc != tokSrc {
				s.mu.Unlock()
				return
			}
			s.tokenExpiry = tok.Expiry
			if changed {
				s.accessTok = tok.AccessToken
				log.Print("Saved refreshed OAuth token")
			}
			s.mu.Unlock()
		}
	}
}

// Events fetches, deduplicates, and sorts events for the day containing t.
func (s *Service) Events(ctx context.Context, t time.Time) ([]*gcal.Event, error) {
	if s == nil {
		return nil, errors.New("calendar service is not initialized")
	}

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

	// Collect per-calendar responses in deterministic config order.
	results := make([]calResult, len(s.calendars))
	var wg sync.WaitGroup
	for i, calID := range s.calendars {
		wg.Add(1)
		go func(idx int, id string) {
			defer wg.Done()
			items, err := fetcher.ListEvents(fetchCtx, id, timeMin, timeMax)
			results[idx] = calResult{cal: id, items: items, err: err}
		}(i, calID)
	}
	wg.Wait()

	var events []*gcal.Event
	var firstErr error
	successCount := 0
	failCount := 0
	for _, res := range results {
		if res.err != nil {
			log.Printf("Calendar fetch failed for %s: %v", res.cal, res.err)
			failCount++
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
	if failCount > 0 {
		log.Printf("Calendar partial failure: %d of %d calendars failed", failCount, len(s.calendars))
	}

	// Deduplicate by event ID, drop nil entries, and exclude out-of-range all-day events.
	targetDate := localDay.Format("2006-01-02")
	seen := make(map[string]struct{}, len(events))
	deduped := events[:0]
	for _, ev := range events {
		if ev == nil || !isAllDayEventOnDate(ev, targetDate) {
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
		return "", fmt.Errorf("fetch calendar events: %w", err)
	}

	now := s.nowFn()
	var lines []string
	todayRenderedIDs := make(map[string]struct{}, len(events))
	for _, ev := range events {
		if li := formatEventLi(ev, now, s.loc); li != "" {
			lines = append(lines, li)
			if ev.Id != "" {
				todayRenderedIDs[ev.Id] = struct{}{}
			}
		}
	}

	if len(lines) < 4 {
		// Anchor at local noon so DST fall-back days still advance to the next calendar date.
		localDay := t.In(s.loc)
		nextDay := time.Date(localDay.Year(), localDay.Month(), localDay.Day(), 12, 0, 0, 0, s.loc).AddDate(0, 0, 1)

		tomorrowCtx, tomorrowCancel := context.WithTimeout(ctx, tomorrowFetchTimeout)
		tomorrowEvents, err := s.Events(tomorrowCtx, nextDay)
		tomorrowCancel()
		if err != nil {
			log.Printf("Fetch tomorrow calendar events failed: %v", err)
		} else {
			var tomorrowLines []string
			for _, ev := range tomorrowEvents {
				if ev != nil && ev.Id != "" {
					if _, dup := todayRenderedIDs[ev.Id]; dup {
						continue
					}
				}
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

// RedirectForToken starts the OAuth authorization flow with a random state token.
func (s *Service) RedirectForToken(w http.ResponseWriter, r *http.Request) {
	if s == nil {
		http.Error(w, "Calendar service unavailable", http.StatusInternalServerError)
		return
	}

	cfg, err := s.loadOAuthConfig()
	if err != nil {
		log.Printf("Load OAuth config failed during redirect: %v", err)
		http.Error(w, "Calendar configuration unavailable", http.StatusInternalServerError)
		return
	}

	state, verifier, err := s.issueStateWithVerifier()
	if err != nil {
		log.Printf("Generate OAuth state failed: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	isHTTPS := isHTTPSRequest(r)
	http.SetCookie(w, &http.Cookie{
		Name:     oauthStateCookieName,
		Value:    state,
		Path:     "/savetoken",
		MaxAge:   int(oauthStateTTL.Seconds()),
		HttpOnly: true,
		Secure:   isHTTPS,
		SameSite: http.SameSiteLaxMode,
	})

	authURL := cfg.AuthCodeURL(
		state,
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"),
		oauth2.S256ChallengeOption(verifier),
	)
	http.Redirect(w, r, authURL, http.StatusSeeOther)
}

// ProcessAuthCode validates the OAuth state token, exchanges the code, and saves the new token.
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

	q := r.URL.Query()
	state := q.Get("state")
	cookie, err := r.Cookie(oauthStateCookieName)
	if err != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(state)) != 1 {
		http.Error(w, "Invalid or expired OAuth state", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     oauthStateCookieName,
		Value:    "",
		Path:     "/savetoken",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   isHTTPSRequest(r),
		SameSite: http.SameSiteLaxMode,
	})

	verifier, ok := s.consumeStateWithVerifier(state)
	if !ok {
		http.Error(w, "Invalid or expired OAuth state", http.StatusBadRequest)
		return
	}

	if oauthErr := strings.TrimSpace(q.Get("error")); oauthErr != "" {
		errDesc := strings.TrimSpace(q.Get("error_description"))
		log.Printf("OAuth authorization failed: error=%q description=%q", oauthErr, errDesc)
		http.Error(w, "Authorization failed", http.StatusBadRequest)
		return
	}

	code := q.Get("code")
	if code == "" {
		http.Error(w, "Missing authorization code", http.StatusBadRequest)
		return
	}

	exchCtx, cancel := context.WithTimeout(r.Context(), fetchTimeout)
	defer cancel()
	tok, err := cfg.Exchange(oauthHTTPContext(exchCtx), code, oauth2.VerifierOption(verifier))
	if err != nil {
		log.Printf("OAuth code exchange failed: %v", err)
		http.Error(w, "Token exchange failed", http.StatusInternalServerError)
		return
	}

	s.setupMu.Lock()
	defer s.setupMu.Unlock()

	s.tokenFileMu.Lock()
	savedTok, saveErr := saveTokenAtomic(s.tokenFile, tok)
	s.tokenFileMu.Unlock()
	if saveErr != nil {
		log.Printf("Save OAuth token failed: %v", saveErr)
		http.Error(w, "Failed to save token", http.StatusInternalServerError)
		return
	}

	if tok.RefreshToken != "" {
		if err := touchFileAtomic(s.refreshTokenFile, s.nowFn()); err != nil {
			log.Printf("Record refresh timestamp failed: %v", err)
		}
	} else {
		log.Print("Warning: Google omitted a refresh token during authorization")
	}

	if err := s.setupServiceLocked(r.Context(), savedTok); err != nil {
		log.Printf("Initialize calendar service failed: %v", err)
		http.Error(w, "Failed to initialize calendar service", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Service) issueState() (string, error) {
	state, _, err := s.issueStateWithVerifier()
	return state, err
}

func (s *Service) issueStateWithVerifier() (string, string, error) {
	buf := make([]byte, oauthStateBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	token := hex.EncodeToString(buf)
	verifier := oauth2.GenerateVerifier()
	now := s.nowFn()

	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.pruneStatesLocked(now)
	if len(s.pendingStates) >= maxPendingStates {
		oldestKey := ""
		var oldestExp time.Time
		for k, entry := range s.pendingStates {
			if oldestKey == "" || entry.expires.Before(oldestExp) {
				oldestKey = k
				oldestExp = entry.expires
			}
		}
		if oldestKey != "" {
			delete(s.pendingStates, oldestKey)
		}
	}
	s.pendingStates[token] = pendingOAuthState{
		verifier: verifier,
		expires:  now.Add(oauthStateTTL),
	}
	return token, verifier, nil
}

func (s *Service) consumeState(candidate string) bool {
	_, ok := s.consumeStateWithVerifier(candidate)
	return ok
}

func (s *Service) consumeStateWithVerifier(candidate string) (string, bool) {
	if candidate == "" {
		return "", false
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
		return "", false
	}
	verifier := s.pendingStates[matchedKey].verifier
	delete(s.pendingStates, matchedKey)
	return verifier, true
}

func (s *Service) pruneStatesLocked(now time.Time) {
	for k, entry := range s.pendingStates {
		if !entry.expires.After(now) {
			delete(s.pendingStates, k)
		}
	}
}

func sortEvents(events []*gcal.Event) {
	sort.Slice(events, func(i, j int) bool {
		a, b := events[i], events[j]

		// Sort non-nil Start values before nil Start values.
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

func isAllDayEventOnDate(event *gcal.Event, targetDate string) bool {
	if event == nil || event.Start == nil || event.Start.Date == "" {
		return true
	}
	if event.Start.Date > targetDate {
		return false
	}
	if event.End != nil && event.End.Date != "" {
		return targetDate < event.End.Date
	}
	return event.Start.Date == targetDate
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
		if start.Before(now) {
			return ""
		}
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

func warnInsecureFilePerms(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	perm := info.Mode().Perm()
	if perm&0077 != 0 {
		log.Printf("Warning: secret file %q has mode %04o. Expected 0600", path, perm)
	}
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

func saveTokenAtomic(path string, token *oauth2.Token) (*oauth2.Token, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("token file path is empty")
	}
	if token == nil {
		return nil, errors.New("oauth token is nil")
	}

	tokCopy := *token
	if tokCopy.RefreshToken == "" {
		if existing, err := readTokenFile(path); err == nil && existing.RefreshToken != "" {
			tokCopy.RefreshToken = existing.RefreshToken
		}
	}

	data, err := json.Marshal(&tokCopy)
	if err != nil {
		return nil, fmt.Errorf("marshal oauth token: %w", err)
	}
	if err := atomicfile.Write(path, data, 0600); err != nil {
		return nil, err
	}
	return &tokCopy, nil
}

func touchFileAtomic(path string, ts time.Time) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("marker file path is empty")
	}
	if err := atomicfile.Write(path, nil, 0600); err != nil {
		return err
	}
	return os.Chtimes(path, ts, ts)
}
