package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// Ollyverse calls the coverage API from the browser, with the X-Olly-User
// header on writes, so both the release and the dev CORS setups must let a
// preflight from it through.
func TestCORSAllowsOllyverse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for name, mw := range map[string]gin.HandlerFunc{
		"release": NewCORSMiddleware(DefaultCORSConfig()),
		"dev":     DevCORSMiddleware(),
	} {
		t.Run(name, func(t *testing.T) {
			r := gin.New()
			r.Use(mw)
			r.PATCH("/api/v1/meta/spec/ENG-1", func(c *gin.Context) { c.Status(http.StatusOK) })
			req := httptest.NewRequest(http.MethodOptions, "/api/v1/meta/spec/ENG-1", nil)
			req.Header.Set("Origin", "https://ollyverse.dev.hiolly.com")
			req.Header.Set("Access-Control-Request-Method", "PATCH")
			req.Header.Set("Access-Control-Request-Headers", "content-type,x-olly-user")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code >= 300 {
				t.Fatalf("preflight refused: %d", w.Code)
			}
			if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://ollyverse.dev.hiolly.com" && got != "*" {
				t.Fatalf("Access-Control-Allow-Origin = %q", got)
			}
		})
	}
}
