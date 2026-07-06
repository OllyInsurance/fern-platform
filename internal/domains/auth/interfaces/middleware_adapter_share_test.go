package interfaces

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/guidewire-oss/fern-platform/pkg/config"
	"github.com/guidewire-oss/fern-platform/pkg/logging"
)

type stubShareValidator struct {
	valid map[string]bool
}

func (s *stubShareValidator) ValidateShareCode(_ context.Context, code string) bool {
	return s.valid[code]
}

func TestIsShareBypassEligible(t *testing.T) {
	cases := []struct {
		method string
		path   string
		want   bool
	}{
		{http.MethodPost, "/query", true},
		{http.MethodGet, "/query", true},
		{http.MethodPut, "/query", false},
		{http.MethodGet, "/web/index.html", true},
		{http.MethodGet, "/web/js/graphql-client.js", true},
		{http.MethodGet, "/docs/readme.md", true},
		{http.MethodPost, "/web/index.html", false},
		{http.MethodGet, "/api/v1/test-runs", false},
		{http.MethodPost, "/api/v1/test-runs", false},
		{http.MethodPost, "/api/v1/share", false},
		{http.MethodGet, "/api/v1/admin/users", false},
		{http.MethodGet, "/", false},
	}
	for _, tc := range cases {
		if got := IsShareBypassEligible(tc.method, tc.path); got != tc.want {
			t.Errorf("IsShareBypassEligible(%s, %s) = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestRequireAuthShareBypass(t *testing.T) {
	gin.SetMode(gin.TestMode)

	logger, err := logging.NewLogger(&config.LoggingConfig{Level: "error", Format: "json", Output: "stdout", Structured: true})
	if err != nil {
		t.Fatalf("failed to create logger: %v", err)
	}

	// Auth ENABLED — the bypass is the only way through without a session.
	cfg := &config.AuthConfig{Enabled: true, OAuth: config.OAuthConfig{Enabled: true}}
	m := NewAuthMiddlewareAdapter(nil, nil, nil, cfg, logger)
	m.SetShareCodeValidator(&stubShareValidator{valid: map[string]bool{"goodcode-1": true}})

	router := gin.New()
	ok := func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"role": c.GetString("role")}) }
	router.POST("/query", m.RequireAuth(), ok)
	router.GET("/web/index.html", m.RequireAuth(), ok)
	router.POST("/api/v1/test-runs", m.RequireAuth(), ok)

	do := func(method, target, shareHeader string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		req.Header.Set("Content-Type", "application/json")
		if shareHeader != "" {
			req.Header.Set("X-Share-Code", shareHeader)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	cases := []struct {
		name       string
		method     string
		target     string
		header     string
		wantStatus int
	}{
		{"valid code via query passes GraphQL", http.MethodPost, "/query?share=goodcode-1", "", http.StatusOK},
		{"valid code via header passes GraphQL", http.MethodPost, "/query", "goodcode-1", http.StatusOK},
		{"valid code passes web assets", http.MethodGet, "/web/index.html?share=goodcode-1", "", http.StatusOK},
		{"invalid code is rejected", http.MethodPost, "/query?share=badcode-99", "", http.StatusUnauthorized},
		{"missing code is rejected", http.MethodPost, "/query", "", http.StatusUnauthorized},
		{"valid code never unlocks REST ingest", http.MethodPost, "/api/v1/test-runs?share=goodcode-1", "", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if w := do(tc.method, tc.target, tc.header); w.Code != tc.wantStatus {
				t.Errorf("%s %s: status = %d, want %d (body: %s)", tc.method, tc.target, w.Code, tc.wantStatus, w.Body.String())
			}
		})
	}

	// The bypass must set a restricted identity, not the anonymous admin one.
	req := httptest.NewRequest(http.MethodPost, "/query?share=goodcode-1", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Body.String() != `{"role":"user"}` {
		t.Errorf("share bypass context = %s, want role user", w.Body.String())
	}
}
