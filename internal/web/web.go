package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"errors"
	htmpl "html/template"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/neoautomata/brd/internal/atomicfile"
	"github.com/neoautomata/brd/internal/auth"
	"github.com/neoautomata/brd/internal/config"
	"github.com/neoautomata/brd/internal/sanitize"
	"github.com/neoautomata/brd/internal/timeutil"
	"github.com/neoautomata/brd/internal/transit"
)

const (
	maxUpdateBodyBytes   = 1 << 20 // 1 MiB
	csrfTokenBytes       = 32
	csrfTokenTTL         = 2 * time.Hour
	maxCSRFTokens        = 1024
	maxUpdateHorizonDays = 365
	updateRegenTimeout   = 25 * time.Second
)

type csrfEntry struct {
	user    string
	expires time.Time
}

//go:embed files/ckengine5.css
var cssData []byte

//go:embed files/edit.tmpl
var editTmplRaw string

//go:embed files/view.tmpl
var viewTmplRaw string

var (
	editTmpl = htmpl.Must(htmpl.New("edit").Parse(editTmplRaw))
	viewTmpl = htmpl.Must(htmpl.New("view").Parse(viewTmplRaw))
)

// CalendarProvider supplies calendar HTML and OAuth handlers.
type CalendarProvider interface {
	RenderHTML(ctx context.Context, t time.Time) (string, error)
	TokenExpiry() string
	RedirectForToken(w http.ResponseWriter, r *http.Request)
	ProcessAuthCode(w http.ResponseWriter, r *http.Request)
}

// TransitProvider supplies upcoming departures for configured lines.
type TransitProvider interface {
	NextDepartures(ctx context.Context, now time.Time, loc *time.Location) []transit.LineDepartures
}

// ImageRegenerator triggers screenshot capture and image conversion for a date.
type ImageRegenerator interface {
	Regenerate(ctx context.Context, dateStr string) error
}

// Dependencies groups all collaborators injected into Server.
type Dependencies struct {
	Config      *config.Config
	Authorizer  *auth.Authorizer
	Calendar    CalendarProvider
	Transit     TransitProvider
	Regenerator ImageRegenerator
	Now         func() time.Time
}

// Server handles HTTP routes for viewing, editing, and serving brd assets.
type Server struct {
	cfg         *config.Config
	authz       *auth.Authorizer
	cal         CalendarProvider
	transit     TransitProvider
	regenerator ImageRegenerator
	nowFn       func() time.Time

	csrfMu     sync.Mutex
	csrfTokens map[string]csrfEntry
}

// NewServer creates a Server with explicit dependencies.
func NewServer(deps Dependencies) *Server {
	nowFn := deps.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	return &Server{
		cfg:         deps.Config,
		authz:       deps.Authorizer,
		cal:         deps.Calendar,
		transit:     deps.Transit,
		regenerator: deps.Regenerator,
		nowFn:       nowFn,
		csrfTokens:  make(map[string]csrfEntry),
	}
}

// Handler returns a dedicated http.ServeMux with all brd routes registered.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.handleView)
	mux.HandleFunc("GET /ckengine5.css", s.handleCSS)

	mux.Handle("GET /edit", s.authz.RequireEditor(http.HandlerFunc(s.handleEdit)))
	mux.Handle("POST /update", s.authz.RequireEditor(http.HandlerFunc(s.handleUpdate)))

	mux.Handle("GET /newtoken", s.authz.RequireAdmin(http.HandlerFunc(s.handleNewToken)))
	mux.Handle("GET /savetoken", s.authz.RequireAdmin(http.HandlerFunc(s.handleSaveToken)))

	mux.HandleFunc("GET /window_screenshot", s.serveDatedFile(0, "image/png", s.cfg.WindowScreenshotPath))
	mux.HandleFunc("GET /screenshot", s.serveDatedFile(0, "image/png", s.cfg.ScreenshotPath))
	mux.HandleFunc("GET /img", s.serveDatedFile(0, "application/octet-stream", s.cfg.ImagePath))
	mux.HandleFunc("GET /img.sha", s.serveDatedFile(0, "text/plain; charset=utf-8", s.cfg.HashPath))

	mux.HandleFunc("GET /screenshot2", s.serveDatedFile(1, "image/png", s.cfg.ScreenshotPath))
	mux.HandleFunc("GET /img2", s.serveDatedFile(1, "application/octet-stream", s.cfg.ImagePath))
	mux.HandleFunc("GET /img2.sha", s.serveDatedFile(1, "text/plain; charset=utf-8", s.cfg.HashPath))

	return mux
}

func (s *Server) handleCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "ckengine5.css", time.Time{}, bytes.NewReader(cssData))
}

func (s *Server) serveDatedFile(dayOffset int, contentType string, pathFn func(string) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		dateStr := s.nowFn().In(s.cfg.Location).AddDate(0, 0, dayOffset).Format("2006-01-02")
		http.ServeFile(w, r, pathFn(dateStr))
	}
}

func (s *Server) handleView(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	now := s.nowFn().In(s.cfg.Location)
	t, dateStr, err := s.parseQueryDate(r.URL.Query().Get("date"), now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	notePath := s.cfg.ContentPath(dateStr)
	noteBytes, err := readNoteIfExists(notePath)
	if err != nil {
		log.Printf("Read note %s failed: %v", notePath, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	var calData htmpl.HTML
	if s.cal != nil {
		rawCal, err := s.cal.RenderHTML(r.Context(), t)
		if err != nil {
			log.Printf("Render calendar for %s failed: %v", dateStr, err)
			calData = htmpl.HTML(`<span style="font-size:24px;"><b>Calendar unavailable</b><br><br></span>`)
		} else {
			calData = htmpl.HTML(rawCal)
		}
	}

	tokExpiry := ""
	if s.cal != nil {
		tokExpiry = s.cal.TokenExpiry()
	}

	lastFullRef, fullRefElapsed := "unknown", "unknown"
	if fi, err := os.Stat(s.cfg.RefreshTokenFile()); err == nil {
		lastFullRef = fi.ModTime().In(s.cfg.Location).Format(time.RFC3339)
		fullRefElapsed = timeutil.FormatElapsed(fi.ModTime(), now)
	}

	var transitLines []transit.LineDepartures
	if s.transit != nil {
		transitLines = s.transit.NextDepartures(r.Context(), now, s.cfg.Location)
	}

	todayMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, s.cfg.Location)
	targetMidnight := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, s.cfg.Location)
	showPrev := targetMidnight.After(todayMidnight)

	prevDate := targetMidnight.AddDate(0, 0, -1).Format("2006-01-02")
	nextDate := targetMidnight.AddDate(0, 0, 1).Format("2006-01-02")

	data := map[string]any{
		"dateHeader":         t.Format("Mon, Jan _2"),
		"showPrev":           showPrev,
		"prevURL":            "/?date=" + prevDate,
		"editURL":            "/edit?date=" + dateStr,
		"nextURL":            "/?date=" + nextDate,
		"weatherWidgetHTML":  htmpl.HTML(s.cfg.WeatherWidgetHTML),
		"calData":            calData,
		"fileContent":        htmpl.HTML(sanitize.HTML(string(noteBytes))),
		"transitLines":       transitLines,
		"tokenExpiry":        tokExpiry,
		"lastFullRefresh":    lastFullRef,
		"fullRefreshElapsed": fullRefElapsed,
	}

	renderTemplate(w, viewTmpl, data)
}

func (s *Server) handleEdit(w http.ResponseWriter, r *http.Request) {
	now := s.nowFn().In(s.cfg.Location)
	t, dateStr, err := s.parseQueryDate(r.URL.Query().Get("date"), now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	notePath := s.cfg.ContentPath(dateStr)
	noteBytes, err := readNoteIfExists(notePath)
	if err != nil {
		log.Printf("Read note %s failed: %v", notePath, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	user := auth.UserFromContext(r.Context())
	csrfToken, err := s.issueCSRFTokenForUser(user)
	if err != nil {
		log.Printf("Generate CSRF token failed: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	data := map[string]any{
		"dateHeader":  t.Format("Mon, Jan _2"),
		"dateStr":     dateStr,
		"backURL":     "/?date=" + dateStr,
		"csrfToken":   csrfToken,
		"fileContent": sanitize.HTML(string(noteBytes)),
	}
	renderTemplate(w, editTmpl, data)
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	setDynamicSecurityHeaders(w)
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUpdateBodyBytes)
	if err := r.ParseForm(); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "Invalid form submission", http.StatusBadRequest)
		return
	}

	user := auth.UserFromContext(r.Context())
	if !s.consumeCSRFTokenForUser(r.PostFormValue("csrf_token"), user) {
		http.Error(w, "Invalid or expired CSRF token", http.StatusForbidden)
		return
	}

	contents, ok := r.PostForm["contents"]
	if !ok || len(contents) == 0 {
		http.Error(w, "Missing contents field", http.StatusBadRequest)
		return
	}

	rawDate := r.PostFormValue("date")
	if rawDate == "" {
		http.Error(w, "Missing date field", http.StatusBadRequest)
		return
	}

	now := s.nowFn().In(s.cfg.Location)
	parsed, err := time.ParseInLocation("2006-01-02", rawDate, s.cfg.Location)
	if err != nil {
		http.Error(w, "Invalid date format", http.StatusBadRequest)
		return
	}
	if timeutil.IsPriorDay(parsed, now, s.cfg.Location) {
		http.Error(w, "Cannot update prior days", http.StatusBadRequest)
		return
	}
	todayMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, s.cfg.Location)
	maxDate := todayMidnight.AddDate(0, 0, maxUpdateHorizonDays)
	if parsed.After(maxDate) {
		http.Error(w, "Cannot update dates more than 365 days ahead", http.StatusBadRequest)
		return
	}
	validDate := parsed.Format("2006-01-02")

	cleanHTML := sanitize.HTML(contents[0])
	notePath := s.cfg.ContentPath(validDate)
	if err := atomicfile.Write(notePath, []byte(cleanHTML), 0600); err != nil {
		log.Printf("Write note %s failed: %v", notePath, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	todayStr := todayMidnight.Format("2006-01-02")
	tomorrowStr := todayMidnight.AddDate(0, 0, 1).Format("2006-01-02")
	if s.regenerator != nil && (validDate == todayStr || validDate == tomorrowStr) {
		regenCtx, cancel := context.WithTimeout(r.Context(), updateRegenTimeout)
		defer cancel()
		if err := s.regenerator.Regenerate(regenCtx, validDate); err != nil {
			log.Printf("Regenerate image for %s failed: %v", validDate, err)
			http.Error(w, "Failed to update display image", http.StatusInternalServerError)
			return
		}
	}

	http.Redirect(w, r, "/?date="+validDate, http.StatusSeeOther)
}

func (s *Server) handleNewToken(w http.ResponseWriter, r *http.Request) {
	setDynamicSecurityHeaders(w)
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cal == nil {
		http.Error(w, "Calendar service unavailable", http.StatusInternalServerError)
		return
	}
	s.cal.RedirectForToken(w, r)
}

func (s *Server) handleSaveToken(w http.ResponseWriter, r *http.Request) {
	setDynamicSecurityHeaders(w)
	if s.cal == nil {
		http.Error(w, "Calendar service unavailable", http.StatusInternalServerError)
		return
	}
	s.cal.ProcessAuthCode(w, r)
}

func (s *Server) parseQueryDate(raw string, now time.Time) (time.Time, string, error) {
	if raw == "" {
		return now, now.Format("2006-01-02"), nil
	}
	parsed, err := time.ParseInLocation("2006-01-02", raw, s.cfg.Location)
	if err != nil {
		return time.Time{}, "", errors.New("Invalid date format")
	}
	if timeutil.IsPriorDay(parsed, now, s.cfg.Location) {
		return time.Time{}, "", errors.New("Cannot access prior days")
	}
	return parsed, parsed.Format("2006-01-02"), nil
}

func (s *Server) issueCSRFToken() (string, error) {
	return s.issueCSRFTokenForUser("")
}

func (s *Server) issueCSRFTokenForUser(user string) (string, error) {
	buf := make([]byte, csrfTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	now := s.nowFn()

	s.csrfMu.Lock()
	defer s.csrfMu.Unlock()
	for k, entry := range s.csrfTokens {
		if !entry.expires.After(now) {
			delete(s.csrfTokens, k)
		}
	}
	// Evict the soonest-expiring entry when the map reaches its hard cap.
	if len(s.csrfTokens) >= maxCSRFTokens {
		oldestKey := ""
		var oldestExp time.Time
		for k, entry := range s.csrfTokens {
			if oldestKey == "" || entry.expires.Before(oldestExp) {
				oldestKey = k
				oldestExp = entry.expires
			}
		}
		if oldestKey != "" {
			delete(s.csrfTokens, oldestKey)
		}
	}
	s.csrfTokens[token] = csrfEntry{
		user:    user,
		expires: now.Add(csrfTokenTTL),
	}
	return token, nil
}

func (s *Server) consumeCSRFTokenForUser(candidate, user string) bool {
	if candidate == "" {
		return false
	}
	now := s.nowFn()

	s.csrfMu.Lock()
	defer s.csrfMu.Unlock()
	matchedKey := ""
	for k, entry := range s.csrfTokens {
		if !entry.expires.After(now) {
			delete(s.csrfTokens, k)
			continue
		}
		if subtle.ConstantTimeCompare([]byte(k), []byte(candidate)) == 1 {
			if entry.user == "" || entry.user == user {
				matchedKey = k
			}
		}
	}
	if matchedKey == "" {
		return false
	}
	delete(s.csrfTokens, matchedKey)
	return true
}

func setDynamicSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
}

func renderTemplate(w http.ResponseWriter, tmpl *htmpl.Template, data map[string]any) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		log.Printf("Execute template %s failed: %v", tmpl.Name(), err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	setDynamicSecurityHeaders(w)
	_, _ = w.Write(buf.Bytes())
}

func readNoteIfExists(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return data, err
}
