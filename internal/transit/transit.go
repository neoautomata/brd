package transit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultCacheDuration  = 10 * time.Minute
	defaultErrorBackoff   = 30 * time.Second
	defaultMaxStaleAge    = 2 * time.Hour
	defaultRequestTimeout = 10 * time.Second
	maxResponseBytes      = 2 << 20 // 2 MiB
	defaultMaxDepartures  = 4
	maxRedirects          = 5
	maxDelayMinutes       = 24 * 60
)

// FlexibleString unmarshals both JSON strings and JSON numbers into a string.
type FlexibleString string

// UnmarshalJSON decodes a string, number, or null JSON value.
func (fs *FlexibleString) UnmarshalJSON(b []byte) error {
	raw := strings.TrimSpace(string(b))
	if raw == "" {
		return errors.New("empty JSON value")
	}
	if raw == "null" {
		*fs = ""
		return nil
	}
	if raw[0] == '{' || raw[0] == '[' {
		return errors.New("cannot decode JSON object or array into FlexibleString")
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*fs = FlexibleString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err == nil {
		*fs = FlexibleString(n.String())
		return nil
	}
	return fmt.Errorf("cannot decode %s into FlexibleString", raw)
}

// Station represents a transit stop in the stationboard API.
type Station struct {
	ID   FlexibleString `json:"id"`
	Name string         `json:"name"`
}

// Prognosis holds real-time status metadata for a departure.
type Prognosis struct {
	Status *string `json:"status"`
}

// Stop describes a scheduled departure from a station.
type Stop struct {
	Station            Station    `json:"station"`
	Departure          string     `json:"departure"`
	DepartureTimestamp int64      `json:"departureTimestamp"`
	Delay              *int       `json:"delay"`
	Prognosis          *Prognosis `json:"prognosis"`
}

// PassItem represents a subsequent stop along a transit route.
type PassItem struct {
	Station Station `json:"station"`
}

// StationboardItem represents one scheduled trip on a stationboard.
type StationboardItem struct {
	Number   FlexibleString `json:"number"`
	Stop     Stop           `json:"stop"`
	PassList []PassItem     `json:"passList"`
}

// StationboardResponse wraps the stationboard list returned by the API.
type StationboardResponse struct {
	Stationboard []StationboardItem `json:"stationboard"`
}

// FilterOptions configures which lines and directions appear in departures.
type FilterOptions struct {
	ExcludedStopID   string
	ExcludedStopName string
	Lines            []string
	MaxDepartures    int
}

// LineDepartures pairs a transit line identifier with its formatted next times.
type LineDepartures struct {
	Line string
	Next string
}

// Service fetches, caches, and formats transit departures.
type Service struct {
	mu            sync.RWMutex
	fetchSem      chan struct{}
	apiURL        string
	cacheDuration time.Duration
	errorBackoff  time.Duration
	maxStaleAge   time.Duration
	httpClient    *http.Client
	filter        FilterOptions
	nowFn         func() time.Time

	cachedResponse *StationboardResponse
	cachedAt       time.Time
	lastErrAt      time.Time
	lastErr        error
}

// NewService constructs a transit Service with explicit dependencies.
func NewService(apiURL string, cacheDuration time.Duration, client *http.Client, filter FilterOptions) *Service {
	if cacheDuration <= 0 {
		cacheDuration = defaultCacheDuration
	}
	if client == nil {
		client = &http.Client{Timeout: defaultRequestTimeout}
	}
	lines := make([]string, len(filter.Lines))
	copy(lines, filter.Lines)
	filter.Lines = lines

	maxStale := defaultMaxStaleAge
	if sixX := 6 * cacheDuration; sixX > maxStale {
		maxStale = sixX
	}

	return &Service{
		fetchSem:      make(chan struct{}, 1),
		apiURL:        strings.TrimSpace(apiURL),
		cacheDuration: cacheDuration,
		errorBackoff:  defaultErrorBackoff,
		maxStaleAge:   maxStale,
		httpClient:    client,
		filter:        filter,
		nowFn:         time.Now,
	}
}

// Fetch returns cached departures when fresh or queries the remote API.
// Concurrent cache misses share one fetch slot to prevent duplicate upstream requests.
// After a transient error, Fetch pauses retries and returns bounded stale data when available.
func (s *Service) Fetch(ctx context.Context) (*StationboardResponse, error) {
	now := s.nowFn()

	s.mu.RLock()
	if resp, err, ok := s.cachedOutcomeLocked(now); ok {
		s.mu.RUnlock()
		return resp, err
	}
	targetURL := s.apiURL
	client := s.httpClient
	s.mu.RUnlock()

	if targetURL == "" {
		return &StationboardResponse{}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	select {
	case s.fetchSem <- struct{}{}:
		defer func() { <-s.fetchSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	now = s.nowFn()
	s.mu.RLock()
	if resp, err, ok := s.cachedOutcomeLocked(now); ok {
		s.mu.RUnlock()
		return resp, err
	}
	stale := s.cachedResponse
	staleAt := s.cachedAt
	s.mu.RUnlock()

	fresh, err := fetchStationboard(ctx, client, targetURL)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		errNow := s.nowFn()
		s.mu.Lock()
		s.lastErrAt = errNow
		s.lastErr = err
		s.mu.Unlock()
		if stale != nil && errNow.Sub(staleAt) <= s.maxStaleAge {
			log.Printf("Transit fetch failed. Using stale cache: %v", err)
			return stale, nil
		}
		return nil, err
	}

	s.mu.Lock()
	s.cachedResponse = fresh
	s.cachedAt = s.nowFn()
	s.lastErrAt = time.Time{}
	s.lastErr = nil
	s.mu.Unlock()

	return fresh, nil
}

func (s *Service) cachedOutcomeLocked(now time.Time) (*StationboardResponse, error, bool) {
	if s.cachedResponse != nil && now.Sub(s.cachedAt) < s.cacheDuration {
		return s.cachedResponse, nil, true
	}
	if !s.lastErrAt.IsZero() && now.Sub(s.lastErrAt) < s.errorBackoff {
		if s.cachedResponse != nil && now.Sub(s.cachedAt) <= s.maxStaleAge {
			return s.cachedResponse, nil, true
		}
		if s.lastErr != nil {
			return nil, s.lastErr, true
		}
	}
	return nil, nil, false
}

// NextDepartures fetches and formats upcoming departures for each configured line.
func (s *Service) NextDepartures(ctx context.Context, now time.Time, loc *time.Location) []LineDepartures {
	lines := s.filter.Lines
	if s.apiURL == "" || len(lines) == 0 {
		return nil
	}

	resp, err := s.Fetch(ctx)
	if err != nil {
		log.Printf("Transit fetch failed: %v", err)
		out := make([]LineDepartures, len(lines))
		for i, line := range lines {
			out[i] = LineDepartures{Line: line, Next: "none"}
		}
		return out
	}

	return FilterAndFormatDepartures(resp.Stationboard, now, loc, s.filter)
}

func validateTargetURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("invalid transit URL %q", rawURL)
	}
	return nil
}

func fetchStationboard(ctx context.Context, client *http.Client, targetURL string) (*StationboardResponse, error) {
	if err := validateTargetURL(targetURL); err != nil {
		return nil, err
	}

	reqCtx := ctx
	if client.Timeout == 0 {
		if _, hasDeadline := ctx.Deadline(); !hasDeadline {
			var cancel context.CancelFunc
			reqCtx, cancel = context.WithTimeout(ctx, defaultRequestTimeout)
			defer cancel()
		}
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create transit request: %w", err)
	}

	reqClient := *client
	origCheckRedirect := reqClient.CheckRedirect
	reqClient.CheckRedirect = func(nextReq *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("transit request exceeded %d redirects", maxRedirects)
		}
		if nextReq.URL == nil || (nextReq.URL.Scheme != "http" && nextReq.URL.Scheme != "https") || nextReq.URL.Host == "" {
			return fmt.Errorf("transit redirect uses invalid URL")
		}
		if origCheckRedirect != nil {
			return origCheckRedirect(nextReq, via)
		}
		return nil
	}

	resp, err := reqClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute transit request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("transit API returned status %d", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, maxResponseBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read transit response: %w", err)
	}
	if int64(len(body)) > maxResponseBytes {
		return nil, fmt.Errorf("transit response exceeds %d bytes", maxResponseBytes)
	}

	var sbResp StationboardResponse
	if err := json.Unmarshal(body, &sbResp); err != nil {
		return nil, fmt.Errorf("decode transit JSON: %w", err)
	}
	return &sbResp, nil
}

type candidate struct {
	depTime       time.Time
	effectiveTime time.Time
	text          string
}

// FilterAndFormatDepartures filters trips and formats upcoming times per line up to opts.MaxDepartures.
func FilterAndFormatDepartures(items []StationboardItem, now time.Time, loc *time.Location, opts FilterOptions) []LineDepartures {
	if loc == nil {
		loc = time.UTC
	}
	nowInLoc := now.In(loc)

	allowedLines := make(map[string]struct{}, len(opts.Lines))
	for _, line := range opts.Lines {
		allowedLines[strings.TrimSpace(line)] = struct{}{}
	}

	excludedID := strings.TrimSpace(opts.ExcludedStopID)
	excludedName := strings.TrimSpace(opts.ExcludedStopName)

	byLine := make(map[string][]candidate, len(opts.Lines))

	for _, item := range items {
		if len(item.PassList) > 1 {
			nextStop := item.PassList[1].Station
			if (excludedID != "" && strings.TrimSpace(string(nextStop.ID)) == excludedID) ||
				(excludedName != "" && strings.TrimSpace(nextStop.Name) == excludedName) {
				continue
			}
		}

		if item.Stop.Prognosis != nil && item.Stop.Prognosis.Status != nil {
			st := strings.ToLower(strings.TrimSpace(*item.Stop.Prognosis.Status))
			if st == "cancelled" || st == "unreachable" {
				continue
			}
		}

		num := strings.TrimSpace(string(item.Number))
		if _, ok := allowedLines[num]; !ok {
			continue
		}

		depTime, hhmm, err := parseDepartureTime(item.Stop.Departure, item.Stop.DepartureTimestamp, nowInLoc, loc)
		if err != nil {
			continue
		}
		effectiveTime := depTime
		entryText := hhmm
		if item.Stop.Delay != nil && *item.Stop.Delay > 0 {
			if *item.Stop.Delay > maxDelayMinutes {
				continue
			}
			effectiveTime = depTime.Add(time.Duration(*item.Stop.Delay) * time.Minute)
			entryText = fmt.Sprintf("%s (+%d)", hhmm, *item.Stop.Delay)
		}
		if !effectiveTime.After(nowInLoc) {
			continue
		}

		byLine[num] = append(byLine[num], candidate{
			depTime:       depTime,
			effectiveTime: effectiveTime,
			text:          entryText,
		})
	}

	out := make([]LineDepartures, len(opts.Lines))
	for i, line := range opts.Lines {
		trimmed := strings.TrimSpace(line)
		cands := byLine[trimmed]
		sort.Slice(cands, func(a, b int) bool {
			if !cands[a].effectiveTime.Equal(cands[b].effectiveTime) {
				return cands[a].effectiveTime.Before(cands[b].effectiveTime)
			}
			return cands[a].depTime.Before(cands[b].depTime)
		})
		out[i] = LineDepartures{
			Line: trimmed,
			Next: formatCandidates(cands, opts.MaxDepartures),
		}
	}
	return out
}

func parseDepartureTime(depStr string, depTs int64, now time.Time, loc *time.Location) (time.Time, string, error) {
	if loc == nil {
		loc = time.UTC
	}
	depStr = strings.TrimSpace(depStr)

	var parsed time.Time
	var err error
	if depTs != 0 {
		parsed = time.Unix(depTs, 0).In(loc)
	} else if depStr != "" {
		formats := []string{
			"2006-01-02T15:04:05-0700",
			"2006-01-02T15:04:05-07:00",
			time.RFC3339,
			"2006-01-02T15:04:05",
			"2006-01-02 15:04:05",
			"15:04",
		}
		for _, f := range formats {
			if f == "15:04" {
				if pt, parseErr := time.ParseInLocation(f, depStr, loc); parseErr == nil {
					nowInLoc := now.In(loc)
					parsed = time.Date(nowInLoc.Year(), nowInLoc.Month(), nowInLoc.Day(), pt.Hour(), pt.Minute(), 0, 0, loc)
					if nowInLoc.Sub(parsed) > 12*time.Hour {
						parsed = parsed.AddDate(0, 0, 1)
					} else if parsed.Sub(nowInLoc) > 12*time.Hour {
						parsed = parsed.AddDate(0, 0, -1)
					}
					err = nil
					break
				} else {
					err = parseErr
				}
			} else if pt, parseErr := time.ParseInLocation(f, depStr, loc); parseErr == nil {
				parsed = pt.In(loc)
				err = nil
				break
			} else {
				err = parseErr
			}
		}
	} else {
		return time.Time{}, "", errors.New("missing departure time")
	}

	if err != nil {
		return time.Time{}, "", err
	}
	return parsed, parsed.In(loc).Format("15:04"), nil
}

func formatCandidates(cands []candidate, maxDepartures int) string {
	if len(cands) == 0 {
		return "none"
	}
	limit := maxDepartures
	if limit <= 0 {
		limit = defaultMaxDepartures
	}
	if len(cands) < limit {
		limit = len(cands)
	}
	parts := make([]string, limit)
	for i := 0; i < limit; i++ {
		parts[i] = cands[i].text
	}
	return strings.Join(parts, ", ")
}
