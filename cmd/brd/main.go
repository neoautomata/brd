package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
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

func main() {
	if err := run(); err != nil {
		log.Printf("Fatal: %v", err)
		os.Exit(1)
	}
}

func run() error {
	cfgFlag := flag.String("config", "", "Path to JSON configuration file")
	flag.Parse()

	cfgPath := resolveConfigPath(*cfgFlag)
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
		log.Printf("Calendar service startup warning: %v", err)
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

	captureSvc := capture.NewService(cfg, nil, nil)
	defer func() {
		if err := captureSvc.Close(); err != nil {
			log.Printf("Stop browser driver failed: %v", err)
		}
	}()

	localLn, err := net.Listen("tcp", cfg.LocalAddr)
	if err != nil {
		return fmt.Errorf("listen on local address %s: %w", cfg.LocalAddr, err)
	}
	defer localLn.Close()

	tsServer := &tsnet.Server{
		Hostname: cfg.TailnetHostname,
		Dir:      cfg.TailscaleDir(),
	}
	defer tsServer.Close()

	tsListener, err := tsServer.Listen("tcp", cfg.TailnetAddr)
	if err != nil {
		return fmt.Errorf("listen on tailnet %s: %w", cfg.TailnetAddr, err)
	}
	defer tsListener.Close()

	lc, err := tsServer.LocalClient()
	if err != nil {
		return fmt.Errorf("create tailscale local client: %w", err)
	}

	tsResolver := auth.TailscaleResolver{
		Client:           lc,
		TrustHeader:      cfg.TrustWebAuthHeader,
		TrustedProxyTags: cfg.TrustedProxyTags,
	}
	tailnetAuth := auth.NewAuthorizer(cfg.AdminUsers, cfg.EditorUsers, tsResolver)
	tailnetWeb := web.NewServer(web.Dependencies{
		Config:      cfg,
		Authorizer:  tailnetAuth,
		Calendar:    calSvc,
		Transit:     transitSvc,
		Regenerator: captureSvc,
	})

	var localResolver auth.IdentityResolver = auth.TailscaleResolver{Client: lc}
	if cfg.TrustWebAuthHeader {
		localResolver = auth.HeaderResolver{}
	}
	localAuth := auth.NewAuthorizer(cfg.AdminUsers, cfg.EditorUsers, localResolver)
	localWeb := web.NewServer(web.Dependencies{
		Config:      cfg,
		Authorizer:  localAuth,
		Calendar:    calSvc,
		Transit:     transitSvc,
		Regenerator: captureSvc,
	})

	tailnetHTTP := newHTTPServer(tailnetWeb.Handler())
	localHTTP := newHTTPServer(localWeb.Handler())

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
				runRegenCycle(rootCtx, cfg, captureSvc)
			}
		}
	}()

	triggerRegen := func() {
		select {
		case regenTrigger <- struct{}{}:
		default:
		}
	}

	// Queue initial image generation without blocking signal or startup error handling.
	triggerRegen()

	ticker := time.NewTicker(cfg.RegenInterval())
	defer ticker.Stop()

	log.Printf("Ready; regenerating images every %s", cfg.RegenInterval())
	for {
		select {
		case <-rootCtx.Done():
			log.Print("Shutdown signal received")
			if err := captureSvc.Close(); err != nil {
				log.Printf("Stop browser capture failed: %v", err)
			}
			regenWG.Wait()
			shutdownServers(tailnetHTTP, localHTTP)
			return nil
		case srvErr := <-errChan:
			stopSignals()
			if err := captureSvc.Close(); err != nil {
				log.Printf("Stop browser capture failed: %v", err)
			}
			regenWG.Wait()
			shutdownServers(tailnetHTTP, localHTTP)
			return srvErr
		case <-ticker.C:
			triggerRegen()
		}
	}
}

func resolveConfigPath(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if envVal := os.Getenv("BRD_CONFIG"); envVal != "" {
		return envVal
	}
	return defaultConfigPath
}

func ensureDataDirs(cfg *config.Config) error {
	dirs := []string{
		cfg.ContentsDir(),
		cfg.ScreenshotsDir(),
		cfg.ImagesDir(),
		cfg.TailscaleDir(),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	return nil
}

func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

func runRegenCycle(ctx context.Context, cfg *config.Config, captureSvc *capture.Service) {
	if ctx.Err() != nil {
		return
	}
	now := time.Now().In(cfg.Location)
	today := now.Format("2006-01-02")
	tomorrow := now.AddDate(0, 0, 1).Format("2006-01-02")

	if err := captureSvc.Regenerate(ctx, today); err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Printf("Regenerate image for %s failed: %v", today, err)
	}
	if ctx.Err() != nil {
		return
	}
	if err := captureSvc.Regenerate(ctx, tomorrow); err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Printf("Regenerate image for %s failed: %v", tomorrow, err)
	}
	if ctx.Err() != nil {
		return
	}
	captureSvc.CleanupOldFiles(now)
}

func shutdownServers(servers ...*http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("HTTP server shutdown error: %v", err)
		}
	}
}
