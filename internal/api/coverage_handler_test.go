package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// The import validates the whole snapshot before it touches the database, so
// these run with no DB: a bad snapshot must be refused, never half-applied or
// silently trimmed (a dropped link reads as an untested criterion).
func TestImportRegistryRejectsBadSnapshots(t *testing.T) {
	gin.SetMode(gin.TestMode)
	spec := `{"key":"ENG-1","title":"t","criteria":[{"id":"AC-1","quote":"q"},{"id":"AC-2","quote":"q"}]}`
	cases := []struct{ name, body, want string }{
		{"unknown criterion", `{"specs":[` + spec + `],"tests":[{"key":"k","links":[{"spec":"ENG-1","criteria":["AC-9"]}]}]}`, "unknown criterion ENG-1 AC-9"},
		{"unknown spec", `{"specs":[` + spec + `],"tests":[{"key":"k","links":[{"spec":"ENG-2","criteria":[]}]}]}`, "unknown spec ENG-2"},
		{"duplicate criterion", `{"specs":[{"key":"ENG-1","criteria":[{"id":"AC-1"},{"id":"AC-1"}]}],"tests":[]}`, "duplicate criterion ENG-1 AC-1"},
		{"duplicate spec", `{"specs":[` + spec + `,` + spec + `],"tests":[]}`, "duplicate spec ENG-1"},
		{"duplicate test", `{"specs":[` + spec + `],"tests":[{"key":"k","links":[]},{"key":"k","links":[]}]}`, "duplicate test k"},
		{"missing spec key", `{"specs":[{"title":"t"}],"tests":[]}`, "a spec has no Key"},
		{"missing criterion id", `{"specs":[{"key":"ENG-1","criteria":[{"quote":"q"}]}],"tests":[]}`, "a criterion of ENG-1 has no id"},
		{"missing test key", `{"specs":[` + spec + `],"tests":[{"links":[]}]}`, "a test has no Key"},
	}
	h := NewCoverageHandler(nil, nil)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(w)
			ctx.Request = httptest.NewRequest(http.MethodPut, "/api/v1/requirements", strings.NewReader(c.body))
			ctx.Request.Header.Set("Content-Type", "application/json")
			h.importRegistry(ctx)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), c.want) {
				t.Fatalf("got %d %s, want 400 containing %q", w.Code, w.Body.String(), c.want)
			}
		})
	}
}
