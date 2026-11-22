package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"tailscale.com/client/tailscale/apitype"
)

// IdentityResolver extracts an authenticated user identifier from an HTTP request.
type IdentityResolver interface {
	ResolveUser(r *http.Request) (string, error)
}

// HeaderResolver reads the user identity from a trusted reverse-proxy header.
type HeaderResolver struct {
	HeaderName string
}

// ResolveUser returns the normalized header value from r.
func (h HeaderResolver) ResolveUser(r *http.Request) (string, error) {
	name := h.HeaderName
	if name == "" {
		name = "X-Webauth-User"
	}
	return normalizeUser(r.Header.Get(name)), nil
}

// WhoIsClient defines the Tailscale LocalClient subset needed for peer lookup.
type WhoIsClient interface {
	WhoIs(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error)
}

// TailscaleResolver resolves the caller identity through Tailscale WhoIs
// and optional trusted reverse-proxy headers.
type TailscaleResolver struct {
	Client           WhoIsClient
	TrustHeader      bool
	HeaderName       string
	TrustedProxyTags []string
}

// ResolveUser resolves the caller identity according to TrustHeader and TrustedProxyTags.
func (t TailscaleResolver) ResolveUser(r *http.Request) (string, error) {
	headerName := t.HeaderName
	if headerName == "" {
		headerName = "X-Webauth-User"
	}

	if t.TrustHeader && len(t.TrustedProxyTags) == 0 {
		if hdr := normalizeUser(r.Header.Get(headerName)); hdr != "" {
			return hdr, nil
		}
		if t.Client == nil {
			return "", nil
		}
		return t.resolveWhoIs(r)
	}

	if t.Client == nil {
		return "", errors.New("tailscale WhoIs client is nil")
	}
	who, err := t.Client.WhoIs(r.Context(), r.RemoteAddr)
	if err != nil {
		return "", err
	}
	if who == nil {
		return "", nil
	}

	if t.TrustHeader && t.hasTrustedProxyTag(who) {
		return normalizeUser(r.Header.Get(headerName)), nil
	}
	return loginName(who), nil
}

func (t TailscaleResolver) resolveWhoIs(r *http.Request) (string, error) {
	who, err := t.Client.WhoIs(r.Context(), r.RemoteAddr)
	if err != nil {
		return "", err
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

// CanEdit reports whether user belongs to either the admin or editor allowlist.
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

func (a *Authorizer) requireRole(next http.Handler, adminOnly bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a == nil || a.resolver == nil {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		user, err := a.resolver.ResolveUser(r)
		if err != nil || user == "" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		allowed := a.CanEdit(user)
		if adminOnly {
			allowed = a.IsAdmin(user)
		}
		if !allowed {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func normalizeUser(user string) string {
	return strings.ToLower(strings.TrimSpace(user))
}
