package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestAuthorizerRolesAndNilReceiver(t *testing.T) {
	var nilAuth *Authorizer
	if nilAuth.IsAdmin("admin@example.com") {
		t.Error("nil Authorizer IsAdmin = true, want false")
	}
	if nilAuth.CanEdit("editor@example.com") {
		t.Error("nil Authorizer CanEdit = true, want false")
	}

	okHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	for _, h := range []http.Handler{nilAuth.RequireAdmin(okHandler), nilAuth.RequireEditor(okHandler)} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/test", nil))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("nil Authorizer status = %d, want 401", rr.Code)
		}
		assertAuthErrorHeaders(t, rr)
	}

	a := NewAuthorizer([]string{"Admin@Example.com", " "}, []string{"Editor@Example.com", ""}, nil)
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

	rr := httptest.NewRecorder()
	a.RequireEditor(okHandler).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/test", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("nil resolver status = %d, want 401", rr.Code)
	}
	assertAuthErrorHeaders(t, rr)
}

func TestUserFromContext(t *testing.T) {
	if got := UserFromContext(nil); got != "" {
		t.Errorf("UserFromContext(nil) = %q, want empty", got)
	}
	if got := UserFromContext(context.Background()); got != "" {
		t.Errorf("UserFromContext(empty) = %q, want empty", got)
	}

	var gotUser string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser = UserFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	a := NewAuthorizer([]string{"admin@example.com"}, nil, HeaderResolver{})
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Webauth-User", "  ADMIN@Example.COM  ")
	rr := httptest.NewRecorder()
	a.RequireAdmin(h).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if gotUser != "admin@example.com" {
		t.Errorf("UserFromContext = %q, want admin@example.com", gotUser)
	}
}

func TestHeaderResolverValidationAndCustomHeader(t *testing.T) {
	t.Run("custom header name", func(t *testing.T) {
		r := HeaderResolver{HeaderName: "X-Forwarded-User"}
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Forwarded-User", "  Editor@Example.com ")
		user, err := r.ResolveUser(req)
		if err != nil {
			t.Fatalf("ResolveUser error: %v", err)
		}
		if user != "editor@example.com" {
			t.Errorf("user = %q, want editor@example.com", user)
		}
	})

	t.Run("multi-value header rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Add("X-Webauth-User", "editor@example.com")
		req.Header.Add("X-Webauth-User", "admin@example.com")
		_, err := (HeaderResolver{}).ResolveUser(req)
		if err == nil || !strings.Contains(err.Error(), "multiple values") {
			t.Errorf("expected multiple values error, got %v", err)
		}
	})

	badValues := []struct {
		name string
		val  string
	}{
		{"comma", "editor@example.com,admin@example.com"},
		{"cr", "editor@example.com\rX-Other: 1"},
		{"lf", "editor@example.com\nX-Other: 1"},
		{"nul", "editor@example.com\x00"},
		{"del", "editor@example.com\x7f"},
	}
	for _, tc := range badValues {
		t.Run("invalid char "+tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header["X-Webauth-User"] = []string{tc.val}
			_, err := (HeaderResolver{}).ResolveUser(req)
			if err == nil || !strings.Contains(err.Error(), "invalid character") {
				t.Errorf("expected invalid character error for %q, got %v", tc.val, err)
			}
		})
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
		headers   []string
		wantCode  int
	}{
		{"admin on editor route", false, []string{"admin@example.com"}, http.StatusOK},
		{"editor on editor route", false, []string{"editor@example.com"}, http.StatusOK},
		{"stranger on editor route", false, []string{"stranger@example.com"}, http.StatusForbidden},
		{"missing user on editor route", false, nil, http.StatusUnauthorized},
		{"multi-value header rejected with 401", false, []string{"editor@example.com", "admin@example.com"}, http.StatusUnauthorized},
		{"comma header rejected with 401", false, []string{"editor@example.com,admin@example.com"}, http.StatusUnauthorized},
		{"admin on admin route", true, []string{"admin@example.com"}, http.StatusOK},
		{"editor on admin route", true, []string{"editor@example.com"}, http.StatusForbidden},
		{"missing user on admin route", true, nil, http.StatusUnauthorized},
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
			for _, hdr := range tc.headers {
				req.Header.Add("X-Webauth-User", hdr)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", rr.Code, tc.wantCode)
			}
			if tc.wantCode == http.StatusUnauthorized || tc.wantCode == http.StatusForbidden {
				assertAuthErrorHeaders(t, rr)
			}
		})
	}
}

func TestTailscaleResolverDirectErrors(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "100.64.0.2:12345"
	req.Header.Set("X-Webauth-User", "admin@example.com")

	t.Run("nil Client fails closed", func(t *testing.T) {
		r := TailscaleResolver{
			Client:           nil,
			TrustHeader:      true,
			TrustedProxyTags: []string{"tag:caddy"},
		}
		_, err := r.ResolveUser(req)
		if err == nil || !strings.Contains(err.Error(), "client is nil") {
			t.Errorf("expected nil client error, got %v", err)
		}
	})

	t.Run("TrustHeader true with empty TrustedProxyTags fails closed", func(t *testing.T) {
		r := TailscaleResolver{
			Client:      mockWhoIs{resp: &apitype.WhoIsResponse{UserProfile: &tailcfg.UserProfile{LoginName: "admin@example.com"}}},
			TrustHeader: true,
		}
		_, err := r.ResolveUser(req)
		if err == nil || !strings.Contains(err.Error(), "trusted proxy tags required when trusting auth header") {
			t.Errorf("expected trusted proxy tags error, got %v", err)
		}
	})

	t.Run("nil WhoIsResponse fails closed", func(t *testing.T) {
		r := TailscaleResolver{
			Client: mockWhoIs{resp: nil, err: nil},
		}
		_, err := r.ResolveUser(req)
		if err == nil || !strings.Contains(err.Error(), "nil response") {
			t.Errorf("expected nil response error, got %v", err)
		}
	})

	t.Run("nil UserProfile returns empty user without error", func(t *testing.T) {
		r := TailscaleResolver{
			Client: mockWhoIs{resp: &apitype.WhoIsResponse{UserProfile: nil}},
		}
		user, err := r.ResolveUser(req)
		if err != nil || user != "" {
			t.Errorf("got (%q, %v), want (\"\", nil)", user, err)
		}
	})

	t.Run("custom HeaderName with trusted proxy tag", func(t *testing.T) {
		customReq := httptest.NewRequest(http.MethodGet, "/", nil)
		customReq.RemoteAddr = "100.64.0.2:12345"
		customReq.Header.Set("X-Proxy-User", "Editor@Example.com")
		r := TailscaleResolver{
			Client: mockWhoIs{resp: &apitype.WhoIsResponse{
				Node: &tailcfg.Node{Tags: []string{"TAG:CADDY"}},
			}},
			TrustHeader:      true,
			HeaderName:       "X-Proxy-User",
			TrustedProxyTags: []string{"tag:caddy"},
		}
		user, err := r.ResolveUser(customReq)
		if err != nil || user != "editor@example.com" {
			t.Errorf("got (%q, %v), want (editor@example.com, nil)", user, err)
		}
	})

	t.Run("trusted proxy multi-value and CRLF headers rejected", func(t *testing.T) {
		r := TailscaleResolver{
			Client: mockWhoIs{resp: &apitype.WhoIsResponse{
				Node: &tailcfg.Node{Tags: []string{"tag:caddy"}},
			}},
			TrustHeader:      true,
			TrustedProxyTags: []string{"tag:caddy"},
		}

		multiReq := httptest.NewRequest(http.MethodGet, "/", nil)
		multiReq.Header.Add("X-Webauth-User", "editor@example.com")
		multiReq.Header.Add("X-Webauth-User", "admin@example.com")
		if _, err := r.ResolveUser(multiReq); err == nil {
			t.Error("expected error for multi-value header")
		}

		crlfReq := httptest.NewRequest(http.MethodGet, "/", nil)
		crlfReq.Header["X-Webauth-User"] = []string{"editor@example.com\r\n"}
		if _, err := r.ResolveUser(crlfReq); err == nil {
			t.Error("expected error for CRLF header")
		}
	})
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
			name:        "TrustHeader=false ignores header and fails closed on tagged node",
			trustHeader: false,
			headerUser:  "admin@example.com",
			whoIsResp: &apitype.WhoIsResponse{
				UserProfile: &tailcfg.UserProfile{LoginName: "editor@example.com"},
				Node:        &tailcfg.Node{Tags: []string{"tag:caddy"}},
			},
			wantCode: http.StatusUnauthorized,
		},
		{
			name:        "TrustHeader=true without tags fails closed",
			trustHeader: true,
			headerUser:  "editor@example.com",
			whoIsResp: &apitype.WhoIsResponse{
				UserProfile: &tailcfg.UserProfile{LoginName: "editor@example.com"},
				Node:        &tailcfg.Node{Tags: []string{"tag:caddy"}},
			},
			wantCode: http.StatusUnauthorized,
		},
		{
			name:             "TrustHeader=true with matching proxy tag accepts header (case-insensitive)",
			trustHeader:      true,
			trustedProxyTags: []string{"TAG:Caddy"},
			headerUser:       "editor@example.com",
			whoIsResp: &apitype.WhoIsResponse{
				UserProfile: &tailcfg.UserProfile{LoginName: "tagged-devices"},
				Node:        &tailcfg.Node{Tags: []string{"tag:CADDY"}},
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
			name:             "TrustHeader=true with proxy tag fails closed on untrusted tagged node",
			trustHeader:      true,
			trustedProxyTags: []string{"tag:caddy"},
			headerUser:       "admin@example.com",
			whoIsResp: &apitype.WhoIsResponse{
				UserProfile: &tailcfg.UserProfile{LoginName: "editor@example.com"},
				Node:        &tailcfg.Node{Tags: []string{"tag:other"}},
			},
			wantCode: http.StatusUnauthorized,
		},
		{
			name:             "TrustHeader=true with proxy tag allows untagged user peer via WhoIs LoginName",
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
		{
			name:        "WhoIs nil response returns 401",
			trustHeader: false,
			whoIsResp:   nil,
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
			if tc.wantCode == http.StatusUnauthorized || tc.wantCode == http.StatusForbidden {
				assertAuthErrorHeaders(t, rr)
			}
		})
	}
}

func assertAuthErrorHeaders(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := rr.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
}
