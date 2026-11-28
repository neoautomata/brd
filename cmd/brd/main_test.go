package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neoautomata/brd/internal/config"
)

func TestParseConfigPath(t *testing.T) {
	t.Parallel()

	envWith := func(val string) func(string) string {
		return func(key string) string {
			if key == "BRD_CONFIG" {
				return val
			}
			return ""
		}
	}

	tests := []struct {
		name           string
		args           []string
		getenv         func(string) string
		wantPath       string
		wantErr        bool
		wantErrIs      error
		wantStderrPart string
	}{
		{
			name:     "default config path when flag and env are empty",
			args:     nil,
			getenv:   envWith(""),
			wantPath: "config.json",
		},
		{
			name:     "BRD_CONFIG env used when flag omitted",
			args:     nil,
			getenv:   envWith("/etc/brd/config.json"),
			wantPath: "/etc/brd/config.json",
		},
		{
			name:     "config flag overrides BRD_CONFIG env",
			args:     []string{"-config", "/custom/config.json"},
			getenv:   envWith("/etc/brd/config.json"),
			wantPath: "/custom/config.json",
		},
		{
			name:     "config flag with equals syntax",
			args:     []string{"--config=/opt/brd.json"},
			getenv:   envWith(""),
			wantPath: "/opt/brd.json",
		},
		{
			name:     "nil getenv falls back to default",
			args:     nil,
			getenv:   nil,
			wantPath: "config.json",
		},
		{
			name:    "unknown flag returns error",
			args:    []string{"-unknown"},
			getenv:  envWith(""),
			wantErr: true,
		},
		{
			name:    "positional argument rejected",
			args:    []string{"extra.json"},
			getenv:  envWith(""),
			wantErr: true,
		},
		{
			name:    "positional argument after valid flag rejected",
			args:    []string{"-config", "/custom/config.json", "extra"},
			getenv:  envWith(""),
			wantErr: true,
		},
		{
			name:           "help flag returns ErrHelp and prints usage",
			args:           []string{"-h"},
			getenv:         envWith(""),
			wantErr:        true,
			wantErrIs:      flag.ErrHelp,
			wantStderrPart: "Path to JSON config file (defaults to BRD_CONFIG or config.json)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stderr bytes.Buffer
			got, err := parseConfigPath(tc.args, tc.getenv, &stderr)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseConfigPath(%v) returned nil error, want error", tc.args)
				}
				if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("parseConfigPath(%v) error = %v, want %v", tc.args, err, tc.wantErrIs)
				}
				if tc.wantStderrPart != "" && !strings.Contains(stderr.String(), tc.wantStderrPart) {
					t.Fatalf("stderr = %q, want substring %q", stderr.String(), tc.wantStderrPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseConfigPath(%v) unexpected error: %v", tc.args, err)
			}
			if got != tc.wantPath {
				t.Fatalf("parseConfigPath(%v) = %q, want %q", tc.args, got, tc.wantPath)
			}
		})
	}
}

func TestRunEarlyErrors(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	if err := run([]string{"-h"}, func(string) string { return "" }, &stderr); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("run(-h) error = %v, want %v", err, flag.ErrHelp)
	}

	missingPath := filepath.Join(t.TempDir(), "does-not-exist.json")
	if err := run([]string{"-config", missingPath}, func(string) string { return "" }, &stderr); err == nil {
		t.Fatal("run with missing config returned nil error, want error")
	}

	if err := run([]string{"unexpected"}, func(string) string { return "" }, &stderr); err == nil {
		t.Fatal("run with positional arg returned nil error, want error")
	}
}

func TestEnsureDataDirs(t *testing.T) {
	t.Parallel()

	t.Run("creates fresh directories with 0700 permissions", func(t *testing.T) {
		t.Parallel()
		base := filepath.Join(t.TempDir(), "brd-data")
		cfg := &config.Config{DataDir: base}

		if err := ensureDataDirs(cfg); err != nil {
			t.Fatalf("ensureDataDirs failed: %v", err)
		}

		expectedDirs := []string{
			cfg.DataDir,
			cfg.ContentsDir(),
			cfg.ScreenshotsDir(),
			cfg.ImagesDir(),
			cfg.TailscaleDir(),
		}
		for _, dir := range expectedDirs {
			info, err := os.Stat(dir)
			if err != nil {
				t.Fatalf("stat %s failed: %v", dir, err)
			}
			if !info.IsDir() {
				t.Fatalf("%s is not a directory", dir)
			}
			if perm := info.Mode().Perm(); perm != 0700 {
				t.Fatalf("%s permissions = %04o, want 0700", dir, perm)
			}
		}

		// Second call must stay idempotent.
		if err := ensureDataDirs(cfg); err != nil {
			t.Fatalf("second ensureDataDirs call failed: %v", err)
		}
	})

	t.Run("tightens existing 0755 directories to 0700", func(t *testing.T) {
		t.Parallel()
		base := filepath.Join(t.TempDir(), "existing-data")
		cfg := &config.Config{DataDir: base}

		dirs := []string{
			cfg.DataDir,
			cfg.ContentsDir(),
			cfg.ScreenshotsDir(),
			cfg.ImagesDir(),
			cfg.TailscaleDir(),
		}
		for _, dir := range dirs {
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatalf("mkdir %s failed: %v", dir, err)
			}
			if err := os.Chmod(dir, 0755); err != nil {
				t.Fatalf("chmod %s failed: %v", dir, err)
			}
		}

		if err := ensureDataDirs(cfg); err != nil {
			t.Fatalf("ensureDataDirs failed: %v", err)
		}

		for _, dir := range dirs {
			info, err := os.Stat(dir)
			if err != nil {
				t.Fatalf("stat %s failed: %v", dir, err)
			}
			if perm := info.Mode().Perm(); perm != 0700 {
				t.Fatalf("%s permissions = %04o, want 0700", dir, perm)
			}
		}
	})

	t.Run("fails when DataDir is an existing regular file", func(t *testing.T) {
		t.Parallel()
		filePath := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(filePath, []byte("x"), 0600); err != nil {
			t.Fatalf("write file failed: %v", err)
		}

		cfg := &config.Config{DataDir: filePath}
		if err := ensureDataDirs(cfg); err == nil {
			t.Fatal("ensureDataDirs returned nil error for regular file DataDir, want error")
		}
	})

	t.Run("fails when a subdirectory path is an existing regular file", func(t *testing.T) {
		t.Parallel()
		base := filepath.Join(t.TempDir(), "data")
		if err := os.MkdirAll(base, 0700); err != nil {
			t.Fatalf("mkdir failed: %v", err)
		}
		cfg := &config.Config{DataDir: base}
		if err := os.WriteFile(cfg.ContentsDir(), []byte("x"), 0600); err != nil {
			t.Fatalf("write file failed: %v", err)
		}

		if err := ensureDataDirs(cfg); err == nil {
			t.Fatal("ensureDataDirs returned nil error when contents path is a file, want error")
		}
	})
}

func TestNewHTTPServer(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := newHTTPServer(ctx, handler)
	if srv.ReadHeaderTimeout != 10*time.Second {
		t.Fatalf("ReadHeaderTimeout = %v, want 10s", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout != 30*time.Second {
		t.Fatalf("ReadTimeout = %v, want 30s", srv.ReadTimeout)
	}
	if srv.WriteTimeout != 60*time.Second {
		t.Fatalf("WriteTimeout = %v, want 60s", srv.WriteTimeout)
	}
	if srv.IdleTimeout != 120*time.Second {
		t.Fatalf("IdleTimeout = %v, want 120s", srv.IdleTimeout)
	}
	if srv.MaxHeaderBytes != 1<<20 {
		t.Fatalf("MaxHeaderBytes = %d, want %d", srv.MaxHeaderBytes, 1<<20)
	}
	if srv.BaseContext == nil {
		t.Fatal("BaseContext is nil, want non-nil")
	}
	if got := srv.BaseContext(nil); got != ctx {
		t.Fatalf("BaseContext returned %v, want %v", got, ctx)
	}

	nilCtxSrv := newHTTPServer(nil, handler)
	if nilCtxSrv.BaseContext == nil || nilCtxSrv.BaseContext(nil) == nil {
		t.Fatal("newHTTPServer(nil, handler) must provide a non-nil BaseContext")
	}
}

func TestNewHTTPServerBaseContextCancellation(t *testing.T) {
	t.Parallel()

	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()

	handlerEntered := make(chan struct{})
	reqCanceled := make(chan struct{})

	srv := newHTTPServer(baseCtx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(handlerEntered)
		<-r.Context().Done()
		close(reqCanceled)
	}))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	defer ln.Close()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(ln)
	}()
	defer shutdownServers(srv)

	client := &http.Client{Timeout: 5 * time.Second}
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		resp, err := client.Get("http://" + ln.Addr().String() + "/")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()

	select {
	case <-handlerEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not receive request in time")
	}

	cancelBase()

	select {
	case <-reqCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("request context did not cancel after BaseContext cancellation")
	}

	shutdownServers(srv)
	<-clientDone
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve returned unexpected error: %v", err)
	}
}

type fakeRegenRunner struct {
	mu           sync.Mutex
	regenerated  []string
	cleanedUpAt  []time.Time
	regenerateFn func(ctx context.Context, dateStr string) error
}

func (f *fakeRegenRunner) Regenerate(ctx context.Context, dateStr string) error {
	f.mu.Lock()
	f.regenerated = append(f.regenerated, dateStr)
	fn := f.regenerateFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, dateStr)
	}
	return nil
}

func (f *fakeRegenRunner) CleanupOldFiles(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanedUpAt = append(f.cleanedUpAt, now)
}

func TestRunRegenCycle(t *testing.T) {
	t.Parallel()

	zurich, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		t.Fatalf("load timezone failed: %v", err)
	}
	// 2026-06-15 23:30 UTC is 2026-06-16 01:30 CEST in Europe/Zurich.
	fixedNow := time.Date(2026, 6, 15, 23, 30, 0, 0, time.UTC)

	t.Run("regenerates today and tomorrow and runs cleanup", func(t *testing.T) {
		t.Parallel()
		runner := &fakeRegenRunner{}
		runRegenCycle(context.Background(), zurich, fixedNow, runner)

		wantDates := []string{"2026-06-16", "2026-06-17"}
		if len(runner.regenerated) != len(wantDates) || runner.regenerated[0] != wantDates[0] || runner.regenerated[1] != wantDates[1] {
			t.Fatalf("regenerated = %v, want %v", runner.regenerated, wantDates)
		}
		if len(runner.cleanedUpAt) != 1 {
			t.Fatalf("cleanup call count = %d, want 1", len(runner.cleanedUpAt))
		}
		if runner.cleanedUpAt[0].Location() != zurich {
			t.Fatalf("cleanup time location = %v, want %v", runner.cleanedUpAt[0].Location(), zurich)
		}
	})

	t.Run("continues after error on today", func(t *testing.T) {
		t.Parallel()
		runner := &fakeRegenRunner{
			regenerateFn: func(_ context.Context, dateStr string) error {
				if dateStr == "2026-06-16" {
					return errors.New("today capture failed")
				}
				return nil
			},
		}
		runRegenCycle(context.Background(), zurich, fixedNow, runner)

		wantDates := []string{"2026-06-16", "2026-06-17"}
		if len(runner.regenerated) != len(wantDates) || runner.regenerated[0] != wantDates[0] || runner.regenerated[1] != wantDates[1] {
			t.Fatalf("regenerated = %v, want %v", runner.regenerated, wantDates)
		}
		if len(runner.cleanedUpAt) != 1 {
			t.Fatalf("cleanup call count = %d, want 1", len(runner.cleanedUpAt))
		}
	})

	t.Run("continues to cleanup after error on tomorrow", func(t *testing.T) {
		t.Parallel()
		runner := &fakeRegenRunner{
			regenerateFn: func(_ context.Context, dateStr string) error {
				if dateStr == "2026-06-17" {
					return errors.New("tomorrow capture failed")
				}
				return nil
			},
		}
		runRegenCycle(context.Background(), zurich, fixedNow, runner)

		if len(runner.regenerated) != 2 {
			t.Fatalf("regenerated = %v, want 2 dates", runner.regenerated)
		}
		if len(runner.cleanedUpAt) != 1 {
			t.Fatalf("cleanup call count = %d, want 1", len(runner.cleanedUpAt))
		}
	})

	t.Run("returns immediately when context is already canceled", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		runner := &fakeRegenRunner{}
		runRegenCycle(ctx, zurich, fixedNow, runner)

		if len(runner.regenerated) != 0 {
			t.Fatalf("regenerated = %v, want empty", runner.regenerated)
		}
		if len(runner.cleanedUpAt) != 0 {
			t.Fatalf("cleanup call count = %d, want 0", len(runner.cleanedUpAt))
		}
	})

	t.Run("stops before tomorrow when context cancels during today", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		runner := &fakeRegenRunner{
			regenerateFn: func(_ context.Context, _ string) error {
				cancel()
				return context.Canceled
			},
		}
		runRegenCycle(ctx, zurich, fixedNow, runner)

		if len(runner.regenerated) != 1 || runner.regenerated[0] != "2026-06-16" {
			t.Fatalf("regenerated = %v, want [2026-06-16]", runner.regenerated)
		}
		if len(runner.cleanedUpAt) != 0 {
			t.Fatalf("cleanup call count = %d, want 0", len(runner.cleanedUpAt))
		}
	})

	t.Run("stops before cleanup when context cancels during tomorrow", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		runner := &fakeRegenRunner{
			regenerateFn: func(_ context.Context, dateStr string) error {
				if dateStr == "2026-06-17" {
					cancel()
					return context.Canceled
				}
				return nil
			},
		}
		runRegenCycle(ctx, zurich, fixedNow, runner)

		if len(runner.regenerated) != 2 {
			t.Fatalf("regenerated = %v, want 2 dates", runner.regenerated)
		}
		if len(runner.cleanedUpAt) != 0 {
			t.Fatalf("cleanup call count = %d, want 0", len(runner.cleanedUpAt))
		}
	})

	t.Run("defaults nil location to UTC", func(t *testing.T) {
		t.Parallel()
		runner := &fakeRegenRunner{}
		runRegenCycle(context.Background(), nil, fixedNow, runner)

		wantDates := []string{"2026-06-15", "2026-06-16"}
		if len(runner.regenerated) != 2 || runner.regenerated[0] != wantDates[0] || runner.regenerated[1] != wantDates[1] {
			t.Fatalf("regenerated = %v, want %v", runner.regenerated, wantDates)
		}
	})
}

func TestShutdownServers(t *testing.T) {
	t.Parallel()

	t.Run("gracefully drains live server and ignores nil entries", func(t *testing.T) {
		t.Parallel()

		reqStarted := make(chan struct{})
		releaseReq := make(chan struct{})

		srv := newHTTPServer(context.Background(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/slow" {
				close(reqStarted)
				<-releaseReq
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		}))

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen failed: %v", err)
		}
		defer ln.Close()

		serveDone := make(chan error, 1)
		go func() {
			serveDone <- srv.Serve(ln)
		}()

		client := &http.Client{Timeout: 5 * time.Second}
		respCh := make(chan *http.Response, 1)
		errCh := make(chan error, 1)
		go func() {
			resp, err := client.Get("http://" + ln.Addr().String() + "/slow")
			if err != nil {
				errCh <- err
				return
			}
			respCh <- resp
		}()

		select {
		case <-reqStarted:
		case <-time.After(3 * time.Second):
			t.Fatal("in-flight request did not reach handler")
		}

		shutdownDone := make(chan struct{})
		go func() {
			shutdownServers(nil, srv)
			close(shutdownDone)
		}()

		close(releaseReq)

		select {
		case err := <-errCh:
			t.Fatalf("in-flight request failed during graceful shutdown: %v", err)
		case resp := <-respCh:
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("in-flight request did not finish")
		}

		select {
		case <-shutdownDone:
		case <-time.After(3 * time.Second):
			t.Fatal("shutdownServers did not return")
		}

		if err := <-serveDone; !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("Serve error = %v, want %v", err, http.ErrServerClosed)
		}
	})

	t.Run("force closes server when shutdown timeout expires", func(t *testing.T) {
		t.Parallel()

		reqStarted := make(chan struct{})
		unblockHandler := make(chan struct{})
		defer close(unblockHandler)

		srv := newHTTPServer(context.Background(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(reqStarted)
			<-unblockHandler
		}))

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen failed: %v", err)
		}
		defer ln.Close()

		serveDone := make(chan error, 1)
		go func() {
			serveDone <- srv.Serve(ln)
		}()

		client := &http.Client{Timeout: 5 * time.Second}
		clientDone := make(chan struct{})
		go func() {
			defer close(clientDone)
			resp, err := client.Get("http://" + ln.Addr().String() + "/")
			if err == nil {
				_ = resp.Body.Close()
			}
		}()

		select {
		case <-reqStarted:
		case <-time.After(3 * time.Second):
			t.Fatal("request did not reach handler")
		}

		shutdownServersWithTimeout(50*time.Millisecond, srv)

		select {
		case err := <-serveDone:
			if !errors.Is(err, http.ErrServerClosed) {
				t.Fatalf("Serve error = %v, want %v", err, http.ErrServerClosed)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Serve did not exit after force close")
		}
		<-clientDone
	})
}
