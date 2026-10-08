package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/guidewire-oss/fern-platform/pkg/config"
	"github.com/guidewire-oss/fern-platform/pkg/logging"
)

// newCachedCovEnv is newCovEnv with the cache on and writes invalidating it,
// wired the way main.go does.
func newCachedCovEnv(t *testing.T) (*covEnv, *CoverageHandler) {
	e := newCovEnv(t)
	h := NewCoverageHandler(e.db, mustLogger())
	// No worker: the test rebuilds by hand so it is deterministic.
	h.cache = &coverageCache{entries: map[string]*coverageEntry{}, building: map[string]chan struct{}{}, wake: make(chan struct{}, 1)}
	r := gin.New()
	r.Use(h.InvalidateOnWrite())
	h.RegisterRoutes(r.Group("/api/v1"))
	e.router = r
	return e, h
}

func mustLogger() *logging.Logger {
	lg, _ := logging.NewLogger(&config.LoggingConfig{Level: "error", Format: "json"})
	return lg
}

func covOwner(t *testing.T, e *covEnv, gz bool) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/requirements/coverage", nil)
	if gz {
		req.Header.Set("Accept-Encoding", "gzip, br")
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("coverage: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.Bytes()
	if gz {
		if w.Header().Get("Content-Encoding") != "gzip" {
			t.Fatalf("asked for gzip, got Content-Encoding %q", w.Header().Get("Content-Encoding"))
		}
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		body, _ = io.ReadAll(zr)
	} else if w.Header().Get("Content-Encoding") != "" {
		t.Fatalf("no gzip asked, got Content-Encoding %q", w.Header().Get("Content-Encoding"))
	}
	var cov struct {
		Specs []struct {
			Metadata json.RawMessage `json:"metadata"`
		} `json:"specs"`
	}
	if err := json.Unmarshal(body, &cov); err != nil {
		t.Fatalf("%v in %s", err, body)
	}
	if len(cov.Specs) != 1 {
		t.Fatalf("specs %d", len(cov.Specs))
	}
	o, _ := dataOf(t, cov.Specs[0].Metadata)["owner"].(string)
	return o
}

// A cached read answers the last build; a write marks it out of date and the
// rebuild picks the write up. Both encodings carry the same body.
func TestCoverageCacheRebuildsAfterWrite(t *testing.T) {
	e, h := newCachedCovEnv(t)
	e.do("PUT", "/api/v1/requirements", registrySnapshot, 200, nil)
	e.do("PUT", "/api/v1/meta/spec/ENG-465", `{"data":{"owner":"sam"}}`, 200, nil)
	if got := covOwner(t, e, false); got != "sam" {
		t.Fatalf("first read owner %q", got)
	}
	if got := covOwner(t, e, true); got != "sam" {
		t.Fatalf("gzip read owner %q", got)
	}

	e.do("PUT", "/api/v1/meta/spec/ENG-465", `{"data":{"owner":"kim"}}`, 200, nil)
	if got := covOwner(t, e, false); got != "sam" {
		t.Fatalf("before the rebuild the last build answers, got %q", got)
	}
	h.rebuildStale()
	if got := covOwner(t, e, true); got != "kim" {
		t.Fatalf("after the rebuild owner %q, want kim", got)
	}

	// A read never makes it out of date; an unchanged window is not rebuilt.
	before := h.cache.entries[coverageKey(DefaultCoverageBranch, DefaultCoverageDays)]
	covOwner(t, e, false)
	h.rebuildStale()
	if h.cache.entries[coverageKey(DefaultCoverageBranch, DefaultCoverageDays)] != before {
		t.Fatal("an unchanged window was rebuilt")
	}
}

// ?fresh=1 builds synchronously.
func TestCoverageCacheFresh(t *testing.T) {
	e, _ := newCachedCovEnv(t)
	e.do("PUT", "/api/v1/requirements", registrySnapshot, 200, nil)
	covOwner(t, e, false)
	e.do("PUT", "/api/v1/meta/spec/ENG-465", `{"data":{"owner":"lee"}}`, 200, nil)
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/requirements/coverage?fresh=1", nil))
	if !strings.Contains(w.Body.String(), `"owner":"lee"`) {
		t.Fatalf("fresh read missed the write: %.300s", w.Body.String())
	}
}

// attachVideos gives each latest result the video of the same-named spec run
// in its test run: the last one carrying a video, in suite then spec order.
func TestAttachVideos(t *testing.T) {
	e := newCovEnv(t)
	for _, q := range []string{
		`CREATE TABLE suite_runs (id INTEGER PRIMARY KEY, test_run_id INTEGER NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE spec_runs (id INTEGER PRIMARY KEY, suite_run_id INTEGER NOT NULL, spec_name TEXT NOT NULL, video_url TEXT, deleted_at DATETIME)`,
		`INSERT INTO suite_runs (id, test_run_id) VALUES (1, 7), (2, 7), (3, 8)`,
		`INSERT INTO spec_runs (id, suite_run_id, spec_name, video_url) VALUES
			(1, 1, 'a', 'https://v/a1'), (2, 2, 'a', 'https://v/a2'), (3, 2, 'a', ''),
			(4, 1, 'b', NULL), (5, 3, 'a', 'https://v/other-run')`,
		`INSERT INTO spec_runs (id, suite_run_id, spec_name, video_url, deleted_at) VALUES (6, 2, 'c', 'https://v/gone', CURRENT_TIMESTAMP)`,
	} {
		if err := e.db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	h := NewCoverageHandler(e.db, mustLogger())
	ta := &coverageTest{Key: "a", Latest: &CoverageResult{TestRunID: 7, SpecName: "a"}}
	tb := &coverageTest{Key: "b", Latest: &CoverageResult{TestRunID: 7, SpecName: "b"}}
	tc := &coverageTest{Key: "c", Latest: &CoverageResult{TestRunID: 7, SpecName: "c"}}
	tn := &coverageTest{Key: "n"}
	if err := h.attachVideos([]*coverageTest{ta, tb, tc, tn}); err != nil {
		t.Fatal(err)
	}
	if ta.Latest.VideoURL != "https://v/a2" || tb.Latest.VideoURL != "" || tc.Latest.VideoURL != "" {
		t.Fatalf("videos a=%q b=%q c=%q", ta.Latest.VideoURL, tb.Latest.VideoURL, tc.Latest.VideoURL)
	}
}
