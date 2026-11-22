package transit

import (
	"context"
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
			// Cancelled departure
			Number: "14",
			Stop:   Stop{Departure: "2026-06-15T10:10:00+0200", Prognosis: &Prognosis{Status: &cancelled}},
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

func TestServiceCacheStaleFallbackAndConcurrency(t *testing.T) {
	var failMode atomic.Bool
	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if failMode.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stationboard":[{"number":9,"stop":{"departure":"2026-06-15T10:12:00+0200"}}]}`))
	}))
	defer srv.Close()

	svc := NewService(srv.URL, 50*time.Millisecond, srv.Client(), FilterOptions{Lines: []string{"9"}})

	// Initial fetch populates cache
	resp, err := svc.Fetch(context.Background())
	if err != nil {
		t.Fatalf("initial Fetch failed: %v", err)
	}
	if len(resp.Stationboard) != 1 {
		t.Fatalf("got %d items, want 1", len(resp.Stationboard))
	}

	// Concurrent cache hits
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.Fetch(context.Background()); err != nil {
				t.Errorf("concurrent Fetch failed: %v", err)
			}
		}()
	}
	wg.Wait()
	if hits.Load() != 1 {
		t.Errorf("expected 1 upstream hit while cache fresh, got %d", hits.Load())
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

func TestServiceRejectsOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("a", maxResponseBytes+10)))
	}))
	defer srv.Close()

	svc := NewService(srv.URL, time.Minute, srv.Client(), FilterOptions{Lines: []string{"9"}})
	if _, err := svc.Fetch(context.Background()); err == nil {
		t.Fatal("expected oversized response error, got nil")
	}
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
