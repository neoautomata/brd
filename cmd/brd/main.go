package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"tailscale.com/tsnet"

	"github.com/neoautomata/brd/internal/auth"
	"github.com/neoautomata/brd/internal/calendar"
	"github.com/neoautomata/brd/internal/capture"
	"github.com/neoautomata/brd/internal/config"
	"github.com/neoautomata/brd/internal/transit"
	"github.com/neoautomata/brd/internal/web"
)

const (
	defaultConfigPath = "config.json"
	shutdownTimeout   = 10 * time.Second
)

type regenRunner interface {
	Regenerate(ctx context.Context, dateStr string) error
	CleanupOldFiles(now time.Time)
}

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		log.Printf("Fatal: %v", err)
		os.Exit(1)
	}
}

func run(args []string, getenv func(string) string, stderr io.Writer) error {
	cfgPath, err := parseConfigPath(args, getenv, stderr)
	if err != nil {
		return err
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("load config %q: %w", cfgPath, err)
	}

	if err := ensureDataDirs(cfg); err != nil {
		return fmt.Errorf("create data directories: %w", err)
	}

	rootCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stopSignals()

	calSvc, err := calendar.NewService(rootCtx, calendar.Options{
		Calendars:        cfg.Calendars,
		Location:         cfg.Location,
		TokenFile:        cfg.TokenFile(),
		RefreshTokenFile: cfg.RefreshTokenFile(),
		ClientSecretFile: cfg.ClientSecretFile(),
	})
	if err != nil {
		log.Printf("Calendar startup warning: %v", err)
	}
	defer calSvc.Close()

	transitSvc := transit.NewService(
		cfg.Transit.StationboardURL,
		cfg.Transit.CacheDuration(),
		&http.Client{Timeout: 10 * time.Second},
		transit.FilterOptions{
			ExcludedStopID:   cfg.Transit.ExcludedStopID,
			ExcludedStopName: cfg.Transit.ExcludedStopName,
			Lines:            cfg.Transit.Lines,
			MaxDepartures:    cfg.Transit.MaxDepartures,
		},
	)

	browserFactory := &capture.ChromedpFactory{
		ExecPath:  cfg.ChromePath,
		NoSandbox: cfg.ChromeNoSandbox,
	}
	captureSvc := capture.NewService(cfg, browserFactory, nil)

	var closeCaptureOnce sync.Once
	closeCapture := func() {
		closeCaptureOnce.Do(func() {
			if err := captureSvc.Close(); err != nil {
				log.Printf("Stop browser capture failed: %v", err)
			}
		})
	}
	defer closeCapture()

	tsServer := &tsnet.Server{
		Hostname: cfg.TailnetHostname,
		Dir:      cfg.TailscaleDir(),
	}
	defer tsServer.Close()

	if _, err := tsServer.Up(rootCtx); err != nil {
		return fmt.Errorf("start tailnet server: %w", err)
	}

	tsListener, err := tsServer.Listen("tcp", cfg.TailnetAddr)
	if err != nil {
		return fmt.Errorf("listen on tailnet %s: %w", cfg.TailnetAddr, err)
	}
	defer tsListener.Close()

	lc, err := tsServer.LocalClient()
	if err != nil {
		return fmt.Errorf("create tailscale local client: %w", err)
	}

	// Open local_addr after tsnet starts so local connections do not stall during startup.
	localLn, err := net.Listen("tcp", cfg.LocalAddr)
	if err != nil {
		return fmt.Errorf("listen on local address %s: %w", cfg.LocalAddr, err)
	}
	defer localLn.Close()

	tsResolver := auth.TailscaleResolver{
		Client:           lc,
		TrustHeader:      cfg.TrustWebAuthHeader,
		TrustedProxyTags: cfg.TrustedProxyTags,
	}
	authorizer := auth.NewAuthorizer(cfg.AdminUsers, cfg.EditorUsers, tsResolver)
	webSrv := web.NewServer(web.Dependencies{
		Config:      cfg,
		Authorizer:  authorizer,
		Calendar:    calSvc,
		Transit:     transitSvc,
		Regenerator: captureSvc,
	})

	// Mount only read-only routes on local_addr so local clients cannot access edit or admin routes.
	tailnetHTTP := newHTTPServer(rootCtx, webSrv.Handler())
	localHTTP := newHTTPServer(rootCtx, webSrv.ReadOnlyHandler())

	errChan := make(chan error, 2)
	go func() {
		log.Printf("Serving tailnet host http://%s%s", cfg.TailnetHostname, cfg.TailnetAddr)
		if err := tailnetHTTP.Serve(tsListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errChan <- fmt.Errorf("tailnet HTTP server: %w", err)
		}
	}()
	go func() {
		log.Printf("Serving local HTTP on http://%s", localLn.Addr().String())
		if err := localHTTP.Serve(localLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errChan <- fmt.Errorf("local HTTP server: %w", err)
		}
	}()

	regenTrigger := make(chan struct{}, 1)
	var regenWG sync.WaitGroup
	regenWG.Add(1)
	go func() {
		defer regenWG.Done()
		for {
			select {
			case <-rootCtx.Done():
				return
			case <-regenTrigger:
				runRegenCycle(rootCtx, cfg.Location, time.Now(), captureSvc)
			}
		}
	}()

	triggerRegen := func() {
		select {
		case regenTrigger <- struct{}{}:
		default:
		}
	}

	// Queue the initial image capture without blocking signals.
	triggerRegen()

	ticker := time.NewTicker(cfg.RegenInterval())
	defer ticker.Stop()

	log.Printf("Server ready. Regenerating images every %s.", cfg.RegenInterval())
	for {
		select {
		case <-rootCtx.Done():
			log.Print("Server received shutdown signal.")
			closeCapture()
			regenWG.Wait()
			shutdownServers(tailnetHTTP, localHTTP)
			return nil
		case srvErr := <-errChan:
			stopSignals()
			closeCapture()
			regenWG.Wait()
			shutdownServers(tailnetHTTP, localHTTP)
			return srvErr
		case <-ticker.C:
			triggerRegen()
		}
	}
}

func parseConfigPath(args []string, getenv func(string) string, stderr io.Writer) (string, error) {
	fs := flag.NewFlagSet("brd", flag.ContinueOnError)
	if stderr != nil {
		fs.SetOutput(stderr)
	}
	cfgFlag := fs.String("config", "", "Path to JSON config file (defaults to BRD_CONFIG or config.json)")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if fs.NArg() > 0 {
		return "", fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	return resolveConfigPath(*cfgFlag, getenv), nil
}

func resolveConfigPath(flagVal string, getenv func(string) string) string {
	if flagVal != "" {
		return flagVal
	}
	if getenv != nil {
		if envVal := getenv("BRD_CONFIG"); envVal != "" {
			return envVal
		}
	}
	return defaultConfigPath
}

func ensureDataDirs(cfg *config.Config) error {
	dirs := []string{
		cfg.DataDir,
		cfg.ContentsDir(),
		cfg.ScreenshotsDir(),
		cfg.ImagesDir(),
		cfg.TailscaleDir(),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		info, err := os.Stat(dir)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("path %q is not a directory", dir)
		}
		if info.Mode().Perm()&0077 != 0 {
			if err := os.Chmod(dir, 0700); err != nil {
				return err
			}
		}
	}
	return nil
}

func newHTTPServer(ctx context.Context, handler http.Handler) *http.Server {
	if ctx == nil {
		ctx = context.Background()
	}
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
}

func runRegenCycle(ctx context.Context, loc *time.Location, now time.Time, runner regenRunner) {
	if ctx.Err() != nil {
		return
	}
	if loc == nil {
		loc = time.UTC
	}
	localNow := now.In(loc)
	today := localNow.Format("2006-01-02")
	tomorrow := localNow.AddDate(0, 0, 1).Format("2006-01-02")

	if err := runner.Regenerate(ctx, today); err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Printf("Regenerate image for %s failed: %v", today, err)
	}
	if ctx.Err() != nil {
		return
	}
	if err := runner.Regenerate(ctx, tomorrow); err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Printf("Regenerate image for %s failed: %v", tomorrow, err)
	}
	if ctx.Err() != nil {
		return
	}
	runner.CleanupOldFiles(localNow)
}

func shutdownServers(servers ...*http.Server) {
	shutdownServersWithTimeout(shutdownTimeout, servers...)
}

func shutdownServersWithTimeout(timeout time.Duration, servers ...*http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var wg sync.WaitGroup
	for _, srv := range servers {
		if srv == nil {
			continue
		}
		wg.Add(1)
		go func(s *http.Server) {
			defer wg.Done()
			if err := s.Shutdown(ctx); err != nil {
				log.Printf("HTTP shutdown error: %v", err)
				if closeErr := s.Close(); closeErr != nil {
					log.Printf("HTTP force-close error: %v", closeErr)
				}
			}
		}(srv)
	}
	wg.Wait()
}
