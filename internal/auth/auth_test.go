package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

type mockWhoIs struct {
	resp *apitype.WhoIsResponse
	err  error
}

func (m mockWhoIs) WhoIs(_ context.Context, _ string) (*apitype.WhoIsResponse, error) {
	return m.resp, m.err
}

func TestAuthorizerRoles(t *testing.T) {
	a := NewAuthorizer([]string{"Admin@Example.com"}, []string{"Editor@Example.com"}, nil)

	tests := []struct {
		user      string
		wantAdmin bool
		wantEdit  bool
	}{
		{"admin@example.com", true, true},
		{"  ADMIN@EXAMPLE.COM ", true, true},
		{"editor@example.com", false, true},
		{"guest@example.com", false, false},
		{"", false, false},
	}

	for _, tc := range tests {
		if got := a.IsAdmin(tc.user); got != tc.wantAdmin {
			t.Errorf("IsAdmin(%q) = %v, want %v", tc.user, got, tc.wantAdmin)
		}
		if got := a.CanEdit(tc.user); got != tc.wantEdit {
			t.Errorf("CanEdit(%q) = %v, want %v", tc.user, got, tc.wantEdit)
		}
	}
}

func TestRequireEditorAndAdminMiddleware(t *testing.T) {
	okHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	a := NewAuthorizer(
		[]string{"admin@example.com"},
		[]string{"editor@example.com"},
		HeaderResolver{},
	)

	tests := []struct {
		name      string
		adminOnly bool
		header    string
		wantCode  int
	}{
		{"admin on editor route", false, "admin@example.com", http.StatusOK},
		{"editor on editor route", false, "editor@example.com", http.StatusOK},
		{"stranger on editor route", false, "stranger@example.com", http.StatusForbidden},
		{"missing user on editor route", false, "", http.StatusUnauthorized},
		{"admin on admin route", true, "admin@example.com", http.StatusOK},
		{"editor on admin route", true, "editor@example.com", http.StatusForbidden},
		{"missing user on admin route", true, "", http.StatusUnauthorized},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var h http.Handler
			if tc.adminOnly {
				h = a.RequireAdmin(okHandler)
			} else {
				h = a.RequireEditor(okHandler)
			}
			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			if tc.header != "" {
				req.Header.Set("X-Webauth-User", tc.header)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", rr.Code, tc.wantCode)
			}
		})
	}
}

func TestTailscaleResolver(t *testing.T) {
	okHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	tests := []struct {
		name             string
		trustHeader      bool
		trustedProxyTags []string
		headerUser       string
		whoIsResp        *apitype.WhoIsResponse
		whoIsErr         error
		wantCode         int
	}{
		{
			name:        "TrustHeader=false uses WhoIs LoginName",
			trustHeader: false,
			headerUser:  "admin@example.com",
			whoIsResp: &apitype.WhoIsResponse{
				UserProfile: &tailcfg.UserProfile{LoginName: "editor@example.com"},
			},
			wantCode: http.StatusOK,
		},
		{
			name:        "TrustHeader=false ignores header and rejects tagged node",
			trustHeader: false,
			headerUser:  "admin@example.com",
			whoIsResp: &apitype.WhoIsResponse{
				UserProfile: &tailcfg.UserProfile{LoginName: "tagged-devices"},
				Node:        &tailcfg.Node{Tags: []string{"tag:caddy"}},
			},
			wantCode: http.StatusForbidden,
		},
		{
			name:        "TrustHeader=true without tags accepts proxy header from tagged node",
			trustHeader: true,
			headerUser:  "editor@example.com",
			whoIsResp: &apitype.WhoIsResponse{
				UserProfile: &tailcfg.UserProfile{LoginName: "tagged-devices"},
				Node:        &tailcfg.Node{Tags: []string{"tag:caddy"}},
			},
			wantCode: http.StatusOK,
		},
		{
			name:        "TrustHeader=true without tags falls back to WhoIs when header absent",
			trustHeader: true,
			headerUser:  "",
			whoIsResp: &apitype.WhoIsResponse{
				UserProfile: &tailcfg.UserProfile{LoginName: "editor@example.com"},
			},
			wantCode: http.StatusOK,
		},
		{
			name:             "TrustHeader=true with matching proxy tag accepts header",
			trustHeader:      true,
			trustedProxyTags: []string{"tag:caddy"},
			headerUser:       "editor@example.com",
			whoIsResp: &apitype.WhoIsResponse{
				UserProfile: &tailcfg.UserProfile{LoginName: "tagged-devices"},
				Node:        &tailcfg.Node{Tags: []string{"tag:caddy"}},
			},
			wantCode: http.StatusOK,
		},
		{
			name:             "TrustHeader=true with matching proxy tag rejects missing header without fallback",
			trustHeader:      true,
			trustedProxyTags: []string{"tag:caddy"},
			headerUser:       "",
			whoIsResp: &apitype.WhoIsResponse{
				UserProfile: &tailcfg.UserProfile{LoginName: "admin@example.com"},
				Node:        &tailcfg.Node{Tags: []string{"tag:caddy"}},
			},
			wantCode: http.StatusUnauthorized,
		},
		{
			name:             "TrustHeader=true with proxy tag rejects spoofed header from untrusted peer",
			trustHeader:      true,
			trustedProxyTags: []string{"tag:caddy"},
			headerUser:       "admin@example.com",
			whoIsResp: &apitype.WhoIsResponse{
				UserProfile: &tailcfg.UserProfile{LoginName: "stranger@example.com"},
				Node:        &tailcfg.Node{Tags: []string{"tag:other"}},
			},
			wantCode: http.StatusForbidden,
		},
		{
			name:             "TrustHeader=true with proxy tag allows non-proxy peer via WhoIs LoginName",
			trustHeader:      true,
			trustedProxyTags: []string{"tag:caddy"},
			headerUser:       "stranger@example.com",
			whoIsResp: &apitype.WhoIsResponse{
				UserProfile: &tailcfg.UserProfile{LoginName: "editor@example.com"},
			},
			wantCode: http.StatusOK,
		},
		{
			name:        "WhoIs error returns 401",
			trustHeader: false,
			whoIsErr:    errors.New("peer not found"),
			wantCode:    http.StatusUnauthorized,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resolver := TailscaleResolver{
				Client:           mockWhoIs{resp: tc.whoIsResp, err: tc.whoIsErr},
				TrustHeader:      tc.trustHeader,
				TrustedProxyTags: tc.trustedProxyTags,
			}
			a := NewAuthorizer([]string{"admin@example.com"}, []string{"editor@example.com"}, resolver)
			req := httptest.NewRequest(http.MethodPost, "/update", nil)
			req.RemoteAddr = "100.64.0.2:12345"
			if tc.headerUser != "" {
				req.Header.Set("X-Webauth-User", tc.headerUser)
			}
			rr := httptest.NewRecorder()
			a.RequireEditor(okHandler).ServeHTTP(rr, req)
			if rr.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", rr.Code, tc.wantCode)
			}
		})
	}
}
