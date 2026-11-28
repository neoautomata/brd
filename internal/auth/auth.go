package auth

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"tailscale.com/client/tailscale/apitype"
)

// IdentityResolver extracts an authenticated user identifier from an HTTP request.
type IdentityResolver interface {
	ResolveUser(r *http.Request) (string, error)
}

// HeaderResolver reads the user identity from a reverse-proxy header.
type HeaderResolver struct {
	HeaderName string
}

// ResolveUser returns the normalized user from the configured header in r.
func (h HeaderResolver) ResolveUser(r *http.Request) (string, error) {
	name := h.HeaderName
	if name == "" {
		name = "X-Webauth-User"
	}
	return readHeaderUser(r.Header, name)
}

// WhoIsClient defines the Tailscale LocalClient subset for peer lookup.
type WhoIsClient interface {
	WhoIs(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error)
}

// TailscaleResolver resolves caller identity through Tailscale WhoIs
// and optional trusted proxy headers.
type TailscaleResolver struct {
	Client           WhoIsClient
	TrustHeader      bool
	HeaderName       string
	TrustedProxyTags []string
}

// ResolveUser resolves caller identity using WhoIs and optional trusted proxy headers.
func (t TailscaleResolver) ResolveUser(r *http.Request) (string, error) {
	if t.Client == nil {
		return "", errors.New("tailscale WhoIs client is nil")
	}
	if t.TrustHeader && len(t.TrustedProxyTags) == 0 {
		return "", errors.New("trusted proxy tags required when trusting auth header")
	}

	who, err := t.Client.WhoIs(r.Context(), r.RemoteAddr)
	if err != nil {
		return "", err
	}
	if who == nil {
		return "", errors.New("tailscale WhoIs returned nil response")
	}

	if t.TrustHeader && t.hasTrustedProxyTag(who) {
		headerName := t.HeaderName
		if headerName == "" {
			headerName = "X-Webauth-User"
		}
		return readHeaderUser(r.Header, headerName)
	}
	if who.Node != nil && len(who.Node.Tags) > 0 {
		return "", nil
	}
	return loginName(who), nil
}

func (t TailscaleResolver) hasTrustedProxyTag(who *apitype.WhoIsResponse) bool {
	if who == nil || who.Node == nil || len(who.Node.Tags) == 0 {
		return false
	}
	for _, nodeTag := range who.Node.Tags {
		normNodeTag := normalizeUser(nodeTag)
		for _, allowed := range t.TrustedProxyTags {
			if normNodeTag == normalizeUser(allowed) {
				return true
			}
		}
	}
	return false
}

func loginName(who *apitype.WhoIsResponse) string {
	if who == nil || who.UserProfile == nil {
		return ""
	}
	return normalizeUser(who.UserProfile.LoginName)
}

func readHeaderUser(headers http.Header, headerName string) (string, error) {
	vals := headers.Values(headerName)
	if len(vals) == 0 {
		return "", nil
	}
	if len(vals) > 1 {
		return "", fmt.Errorf("header %s has multiple values", headerName)
	}
	raw := vals[0]
	for _, r := range raw {
		if r == ',' || r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("header %s contains invalid character", headerName)
		}
	}
	return normalizeUser(raw), nil
}

// Authorizer enforces admin and editor access control on HTTP handlers.
type Authorizer struct {
	admins   map[string]struct{}
	editors  map[string]struct{}
	resolver IdentityResolver
}

// NewAuthorizer builds an Authorizer from admin and editor user lists.
func NewAuthorizer(admins, editors []string, resolver IdentityResolver) *Authorizer {
	adminSet := make(map[string]struct{}, len(admins))
	for _, u := range admins {
		if norm := normalizeUser(u); norm != "" {
			adminSet[norm] = struct{}{}
		}
	}
	editorSet := make(map[string]struct{}, len(editors))
	for _, u := range editors {
		if norm := normalizeUser(u); norm != "" {
			editorSet[norm] = struct{}{}
		}
	}
	return &Authorizer{
		admins:   adminSet,
		editors:  editorSet,
		resolver: resolver,
	}
}

// IsAdmin reports whether user belongs to the admin allowlist.
func (a *Authorizer) IsAdmin(user string) bool {
	if a == nil {
		return false
	}
	_, ok := a.admins[normalizeUser(user)]
	return ok
}

// CanEdit reports whether user belongs to the admin or editor allowlist.
func (a *Authorizer) CanEdit(user string) bool {
	if a == nil {
		return false
	}
	norm := normalizeUser(user)
	if _, ok := a.admins[norm]; ok {
		return true
	}
	_, ok := a.editors[norm]
	return ok
}

// RequireAdmin wraps next so only admin users can invoke it.
func (a *Authorizer) RequireAdmin(next http.Handler) http.Handler {
	return a.requireRole(next, true)
}

// RequireEditor wraps next so admin and editor users can invoke it.
func (a *Authorizer) RequireEditor(next http.Handler) http.Handler {
	return a.requireRole(next, false)
}

type userContextKey struct{}

// UserFromContext returns the normalized user that Authorizer middleware sets in ctx.
func UserFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	user, _ := ctx.Value(userContextKey{}).(string)
	return user
}

func (a *Authorizer) requireRole(next http.Handler, adminOnly bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a == nil || a.resolver == nil {
			writeAuthError(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		user, err := a.resolver.ResolveUser(r)
		if err != nil {
			log.Printf("Resolve user identity failed: %v", err)
			writeAuthError(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if user == "" {
			writeAuthError(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		allowed := a.CanEdit(user)
		if adminOnly {
			allowed = a.IsAdmin(user)
		}
		if !allowed {
			writeAuthError(w, "Forbidden", http.StatusForbidden)
			return
		}
		ctx := context.WithValue(r.Context(), userContextKey{}, normalizeUser(user))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func writeAuthError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	http.Error(w, msg, code)
}

func normalizeUser(user string) string {
	return strings.ToLower(strings.TrimSpace(user))
}
