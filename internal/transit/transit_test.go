package transit

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFilterAndFormatDepartures(t *testing.T) {
	zurich, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		t.Fatalf("load Europe/Zurich: %v", err)
	}

	now := time.Date(2026, 6, 15, 10, 0, 0, 0, zurich)
	delay3 := 3
	cancelled := "cancelled"
	unreachable := " UNREACHABLE "

	// 10:15 CEST in Unix seconds: 2026-06-15 08:15 UTC
	ts1015 := time.Date(2026, 6, 15, 8, 15, 0, 0, time.UTC).Unix()
	// 10:05 CEST in Unix seconds
	ts1005 := time.Date(2026, 6, 15, 8, 5, 0, 0, time.UTC).Unix()
	// 09:55 CEST (past)
	ts0955 := time.Date(2026, 6, 15, 7, 55, 0, 0, time.UTC).Unix()

	items := []StationboardItem{
		{
			Number: "9",
			Stop:   Stop{DepartureTimestamp: ts1015, Delay: &delay3},
			PassList: []PassItem{
				{Station: Station{ID: "1", Name: "Origin"}},
				{Station: Station{ID: "2", Name: "Downtown"}},
			},
		},
		{
			Number: "9",
			Stop:   Stop{DepartureTimestamp: ts1005},
			PassList: []PassItem{
				{Station: Station{ID: "1", Name: "Origin"}},
				{Station: Station{ID: "2", Name: "Downtown"}},
			},
		},
		{
			// Excluded direction by stop ID
			Number: "9",
			Stop:   Stop{Departure: "2026-06-15T10:08:00+0200"},
			PassList: []PassItem{
				{Station: Station{ID: "1", Name: "Origin"}},
				{Station: Station{ID: "8503023", Name: "Other"}},
			},
		},
		{
			// Excluded direction by stop name
			Number: "9",
			Stop:   Stop{Departure: "2026-06-15T10:09:00+0200"},
			PassList: []PassItem{
				{Station: Station{ID: "1", Name: "Origin"}},
				{Station: Station{ID: "9999999", Name: " Excluded Stop "}},
			},
		},
		{
			// Cancelled departure
			Number: "14",
			Stop:   Stop{Departure: "2026-06-15T10:10:00+0200", Prognosis: &Prognosis{Status: &cancelled}},
		},
		{
			// Unreachable departure
			Number: "14",
			Stop:   Stop{Departure: "2026-06-15T10:12:00+0200", Prognosis: &Prognosis{Status: &unreachable}},
		},
		{
			// Past departure
			Number: "14",
			Stop:   Stop{DepartureTimestamp: ts0955},
		},
		{
			// UTC timestamp converted to Zurich local time (08:20 UTC -> 10:20 CEST)
			Number: "14",
			Stop:   Stop{Departure: "2026-06-15T08:20:00Z"},
		},
	}

	got := FilterAndFormatDepartures(items, now, zurich, FilterOptions{
		ExcludedStopID:   "8503023",
		ExcludedStopName: "Excluded Stop",
		Lines:            []string{"9", "14"},
	})

	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].Line != "9" || got[0].Next != "10:05, 10:15 (+3)" {
		t.Errorf("line 9 = %+v, want 10:05, 10:15 (+3)", got[0])
	}
	if got[1].Line != "14" || got[1].Next != "10:20" {
		t.Errorf("line 14 = %+v, want 10:20", got[1])
	}
}

func TestFlexibleStringUnmarshalJSON(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    FlexibleString
		wantErr bool
	}{
		{"string", `"S12"`, "S12", false},
		{"integer", `8503023`, "8503023", false},
		{"float", `12.5`, "12.5", false},
		{"null", `null`, "", false},
		{"object rejected", `{"id":1}`, "", true},
		{"array rejected", `[1,2]`, "", true},
		{"boolean rejected", `true`, "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var fs FlexibleString
			err := json.Unmarshal([]byte(tc.input), &fs)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %s, got %q", tc.input, fs)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %s: %v", tc.input, err)
			}
			if fs != tc.want {
				t.Errorf("got %q, want %q", fs, tc.want)
			}
		})
	}
}

func TestDepartureTimeFormatsDSTAndExtremeDelay(t *testing.T) {
	zurich, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		t.Fatalf("load Europe/Zurich: %v", err)
	}

	// Non-RFC3339 formats and surrounding whitespace
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, zurich)
	extremeDelay := math.MaxInt
	items := []StationboardItem{
		{Number: "9", Stop: Stop{Departure: " 2026-06-15T10:05:00 "}},
		{Number: "9", Stop: Stop{Departure: "2026-06-15 10:10:00"}},
		{Number: "9", Stop: Stop{Departure: " 10:15 "}},
		{Number: "9", Stop: Stop{Departure: "2026-06-15T10:20:00+0200", Delay: &extremeDelay}},
	}
	got := FilterAndFormatDepartures(items, now, zurich, FilterOptions{Lines: []string{"9"}})
	if len(got) != 1 || got[0].Next != "10:05, 10:10, 10:15" {
		t.Errorf("formats/extreme delay = %+v, want 10:05, 10:10, 10:15", got)
	}

	// Post-midnight past departure: at 00:05, "23:55" rolls backward to yesterday (dropped),
	// while "23:55" with +15 delay rolls to 00:10 (upcoming), and "00:12" stays today.
	earlyNow := time.Date(2026, 6, 16, 0, 5, 0, 0, zurich)
	delay15 := 15
	earlyItems := []StationboardItem{
		{Number: "14", Stop: Stop{Departure: "23:55"}},
		{Number: "14", Stop: Stop{Departure: "23:55", Delay: &delay15}},
		{Number: "14", Stop: Stop{Departure: " 00:12 "}},
	}
	gotEarly := FilterAndFormatDepartures(earlyItems, earlyNow, zurich, FilterOptions{Lines: []string{"14"}})
	if len(gotEarly) != 1 || gotEarly[0].Next != "23:55 (+15), 00:12" {
		t.Errorf("post-midnight rollover = %+v, want 23:55 (+15), 00:12", gotEarly)
	}

	// DST fall-back repeated hour in Europe/Zurich (2026-10-25):
	// 00:30 UTC is 02:30 CEST (+0200), 01:15 UTC is 02:15 CET (+0100), 01:30 UTC is 02:30 CET (+0100).
	dstNow := time.Date(2026, 10, 25, 0, 45, 0, 0, time.UTC).In(zurich) // 02:45 CEST
	dstItems := []StationboardItem{
		{Number: "9", Stop: Stop{Departure: "2026-10-25T02:30:00+02:00"}}, // past (00:30 UTC)
		{Number: "9", Stop: Stop{Departure: "2026-10-25T02:15:00+01:00"}}, // upcoming (01:15 UTC)
		{Number: "9", Stop: Stop{Departure: "2026-10-25T02:30:00+01:00"}}, // upcoming (01:30 UTC)
	}
	gotDST := FilterAndFormatDepartures(dstItems, dstNow, zurich, FilterOptions{Lines: []string{"9"}})
	if len(gotDST) != 1 || gotDST[0].Next != "02:15, 02:30" {
		t.Errorf("DST fall-back departures = %+v, want 02:15, 02:30", gotDST)
	}
}

func TestServiceNextDeparturesHappyPathAndFallback(t *testing.T) {
	var failMode atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if failMode.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stationboard":[{"number":9,"stop":{"departure":"2026-06-15T10:12:00Z"}}]}`))
	}))
	defer srv.Close()

	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)

	svc := NewService(srv.URL, time.Minute, srv.Client(), FilterOptions{Lines: []string{"9", "14"}})
	got := svc.NextDepartures(context.Background(), now, time.UTC)
	if len(got) != 2 || got[0].Line != "9" || got[0].Next != "10:12" || got[1].Line != "14" || got[1].Next != "none" {
		t.Fatalf("NextDepartures happy path = %+v", got)
	}

	// Error fallback with empty cache returns "none" for each line.
	failMode.Store(true)
	errSvc := NewService(srv.URL, time.Minute, srv.Client(), FilterOptions{Lines: []string{"9", "14"}})
	gotErr := errSvc.NextDepartures(context.Background(), now, time.UTC)
	if len(gotErr) != 2 || gotErr[0].Next != "none" || gotErr[1].Next != "none" {
		t.Fatalf("NextDepartures error fallback = %+v, want all none", gotErr)
	}
}

func TestServiceCacheStaleFallbackAndConcurrency(t *testing.T) {
	var failMode atomic.Bool
	var hits atomic.Int32
	gate := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		<-gate
		if failMode.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stationboard":[{"number":9,"stop":{"departure":"2026-06-15T10:12:00+0200"}}]}`))
	}))
	defer srv.Close()

	svc := NewService(srv.URL, 50*time.Millisecond, srv.Client(), FilterOptions{Lines: []string{"9"}})

	// Concurrent cache misses share a single upstream hit.
	const callers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, err := svc.Fetch(context.Background())
			if err != nil {
				t.Errorf("concurrent cache-miss Fetch failed: %v", err)
				return
			}
			if len(resp.Stationboard) != 1 {
				t.Errorf("got %d items, want 1", len(resp.Stationboard))
			}
		}()
	}
	close(start)
	time.Sleep(15 * time.Millisecond)
	close(gate)
	wg.Wait()
	if hits.Load() != 1 {
		t.Errorf("expected 1 upstream hit across %d concurrent cache misses, got %d", callers, hits.Load())
	}

	// Expire cache and simulate upstream failure -> stale cache returned
	svc.mu.Lock()
	svc.cachedAt = time.Now().Add(-time.Hour)
	svc.mu.Unlock()
	failMode.Store(true)

	staleResp, err := svc.Fetch(context.Background())
	if err != nil {
		t.Fatalf("expected stale fallback without error, got %v", err)
	}
	if len(staleResp.Stationboard) != 1 {
		t.Errorf("expected stale cached item, got %d", len(staleResp.Stationboard))
	}
}

func TestServiceEmptyCacheBackoffAndCancelDoesNotPoison(t *testing.T) {
	var mode atomic.Int32 // 0 = 502 error, 1 = block until ctx done, 2 = 200 ok
	var hits atomic.Int32
	started := make(chan struct{}, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch mode.Load() {
		case 0:
			w.WriteHeader(http.StatusBadGateway)
		case 1:
			select {
			case started <- struct{}{}:
			default:
			}
			<-r.Context().Done()
		case 2:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"stationboard":[{"number":9,"stop":{"departure":"2026-06-15T10:12:00Z"}}]}`))
		}
	}))
	defer srv.Close()

	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	svc := NewService(srv.URL, 5*time.Minute, srv.Client(), FilterOptions{Lines: []string{"9"}})
	svc.nowFn = func() time.Time { return now }

	// Empty-cache failure caches lastErr and fast-fails within errorBackoff without hitting upstream again.
	mode.Store(0)
	if _, err := svc.Fetch(context.Background()); err == nil {
		t.Fatal("expected error on first upstream failure")
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", hits.Load())
	}

	now = now.Add(5 * time.Second)
	if _, err := svc.Fetch(context.Background()); err == nil {
		t.Fatal("expected cached error during backoff with empty cache")
	}
	if hits.Load() != 1 {
		t.Errorf("empty-cache error backoff failed: hits = %d, want 1", hits.Load())
	}

	// After errorBackoff expires, caller-canceled context must NOT poison backoff.
	now = now.Add(defaultErrorBackoff + time.Second)
	mode.Store(1)
	cancelCtx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := svc.Fetch(cancelCtx)
		errCh <- err
	}()
	<-started

	// A second caller waiting for fetchSem aborts immediately when its context is canceled.
	waiterCtx, cancelWaiter := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelWaiter()
	if _, err := svc.Fetch(waiterCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiter err = %v, want context.DeadlineExceeded", err)
	}

	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled fetch err = %v, want context.Canceled", err)
	}

	// Immediate next call with live context hits upstream and succeeds because cancel did not poison backoff.
	mode.Store(2)
	resp, err := svc.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch after caller cancel failed: %v", err)
	}
	if len(resp.Stationboard) != 1 {
		t.Errorf("got %d items, want 1", len(resp.Stationboard))
	}
}

func TestServiceValidationMalformedJSONAndRedirects(t *testing.T) {
	t.Run("invalid URL schemes rejected", func(t *testing.T) {
		for _, badURL := range []string{"file:///etc/passwd", "ftp://example.com/api", "http://"} {
			svc := NewService(badURL, time.Minute, nil, FilterOptions{Lines: []string{"9"}})
			if _, err := svc.Fetch(context.Background()); err == nil {
				t.Errorf("expected error for invalid URL %q", badURL)
			}
		}
	})

	t.Run("malformed JSON returns decode error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"stationboard": [invalid`))
		}))
		defer srv.Close()

		svc := NewService(srv.URL, time.Minute, srv.Client(), FilterOptions{Lines: []string{"9"}})
		if _, err := svc.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "decode transit JSON") {
			t.Errorf("err = %v, want decode transit JSON error", err)
		}
	})

	t.Run("redirect to non-http scheme rejected", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "ftp://example.com/data", http.StatusFound)
		}))
		defer srv.Close()

		svc := NewService(srv.URL, time.Minute, srv.Client(), FilterOptions{Lines: []string{"9"}})
		if _, err := svc.Fetch(context.Background()); err == nil {
			t.Error("expected error for redirect to ftp scheme")
		}
	})

	t.Run("excessive redirects rejected", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/loop", http.StatusFound)
		}))
		defer srv.Close()

		svc := NewService(srv.URL, time.Minute, srv.Client(), FilterOptions{Lines: []string{"9"}})
		if _, err := svc.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "redirects") {
			t.Errorf("err = %v, want redirects error", err)
		}
	})

	t.Run("oversized body rejected", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(strings.Repeat("a", maxResponseBytes+10)))
		}))
		defer srv.Close()

		svc := NewService(srv.URL, time.Minute, srv.Client(), FilterOptions{Lines: []string{"9"}})
		if _, err := svc.Fetch(context.Background()); err == nil {
			t.Fatal("expected oversized response error, got nil")
		}
	})
}

func TestFilterAndFormatDeparturesMaxLimit(t *testing.T) {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	items := []StationboardItem{
		{Number: "9", Stop: Stop{Departure: "2026-06-15T10:05:00Z"}},
		{Number: "9", Stop: Stop{Departure: "2026-06-15T10:10:00Z"}},
		{Number: "9", Stop: Stop{Departure: "2026-06-15T10:15:00Z"}},
		{Number: "9", Stop: Stop{Departure: "2026-06-15T10:20:00Z"}},
		{Number: "9", Stop: Stop{Departure: "2026-06-15T10:25:00Z"}},
		{Number: "9", Stop: Stop{Departure: "2026-06-15T10:30:00Z"}},
	}

	tests := []struct {
		name          string
		maxDepartures int
		want          string
	}{
		{"zero defaults to 4", 0, "10:05, 10:10, 10:15, 10:20"},
		{"negative defaults to 4", -3, "10:05, 10:10, 10:15, 10:20"},
		{"limit 1", 1, "10:05"},
		{"limit 2", 2, "10:05, 10:10"},
		{"limit 6 returns all 6", 6, "10:05, 10:10, 10:15, 10:20, 10:25, 10:30"},
		{"limit exceeding available count returns available", 10, "10:05, 10:10, 10:15, 10:20, 10:25, 10:30"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FilterAndFormatDepartures(items, now, time.UTC, FilterOptions{
				Lines:         []string{"9"},
				MaxDepartures: tc.maxDepartures,
			})
			if len(got) != 1 || got[0].Next != tc.want {
				t.Errorf("got %+v, want %q", got, tc.want)
			}
		})
	}
}

func TestDelayedDepartureEffectiveTimeAndMidnightRollover(t *testing.T) {
	zurich, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		t.Fatalf("load Europe/Zurich: %v", err)
	}

	// Scheduled 14:00 with +5 delay -> effective 14:05, still upcoming at 14:01.
	// Scheduled 14:03 on time -> effective 14:03, sorts before 14:00 (+5).
	now := time.Date(2026, 6, 15, 14, 1, 0, 0, zurich)
	delay5 := 5
	delay1 := 1
	items := []StationboardItem{
		{Number: "9", Stop: Stop{Departure: "2026-06-15T14:00:00+0200", Delay: &delay5}},
		{Number: "9", Stop: Stop{Departure: "2026-06-15T14:03:00+0200"}},
		{Number: "9", Stop: Stop{Departure: "2026-06-15T13:59:00+0200", Delay: &delay1}}, // effective 14:00 <= 14:01 -> dropped
	}
	got := FilterAndFormatDepartures(items, now, zurich, FilterOptions{Lines: []string{"9"}})
	if len(got) != 1 || got[0].Next != "14:03, 14:00 (+5)" {
		t.Errorf("delayed departures = %+v, want 14:03, 14:00 (+5)", got)
	}

	// "15:04" fallback across midnight: at 23:55, "00:05" rolls to next day; "23:50" stays past.
	lateNow := time.Date(2026, 6, 15, 23, 55, 0, 0, zurich)
	lateItems := []StationboardItem{
		{Number: "14", Stop: Stop{Departure: "23:50"}},
		{Number: "14", Stop: Stop{Departure: "00:05"}},
	}
	gotLate := FilterAndFormatDepartures(lateItems, lateNow, zurich, FilterOptions{Lines: []string{"14"}})
	if len(gotLate) != 1 || gotLate[0].Next != "00:05" {
		t.Errorf("midnight rollover = %+v, want 00:05", gotLate)
	}
}

func TestServiceErrorBackoffMaxStaleAndUnconfigured(t *testing.T) {
	// Unconfigured URL returns nil from NextDepartures even if lines are present.
	emptySvc := NewService("", time.Minute, nil, FilterOptions{Lines: []string{"9"}})
	if got := emptySvc.NextDepartures(context.Background(), time.Now(), time.UTC); got != nil {
		t.Errorf("unconfigured NextDepartures = %+v, want nil", got)
	}

	var failMode atomic.Bool
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if failMode.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stationboard":[{"number":9,"stop":{"departure":"2026-06-15T10:12:00Z"}}]}`))
	}))
	defer srv.Close()

	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	svc := NewService(srv.URL, 5*time.Minute, srv.Client(), FilterOptions{Lines: []string{"9"}})
	svc.nowFn = func() time.Time { return now }

	if _, err := svc.Fetch(context.Background()); err != nil {
		t.Fatalf("initial Fetch: %v", err)
	}

	// Expire cache and trigger failure; subsequent calls within errorBackoff must not hammer upstream.
	failMode.Store(true)
	now = now.Add(10 * time.Minute)
	if _, err := svc.Fetch(context.Background()); err != nil {
		t.Fatalf("expected stale fallback on first error, got %v", err)
	}
	hitsAfterFirstErr := hits.Load()
	now = now.Add(5 * time.Second)
	if _, err := svc.Fetch(context.Background()); err != nil {
		t.Fatalf("expected error backoff stale fallback, got %v", err)
	}
	if hits.Load() != hitsAfterFirstErr {
		t.Errorf("upstream hits during backoff = %d, want %d", hits.Load(), hitsAfterFirstErr)
	}

	// Past maxStaleAge, error is returned instead of indefinitely stale data.
	now = now.Add(3 * time.Hour)
	if _, err := svc.Fetch(context.Background()); err == nil {
		t.Error("expected error once cache exceeds maxStaleAge")
	}
}
