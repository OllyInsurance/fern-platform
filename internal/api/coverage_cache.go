package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

// The coverage roll-up joins every registry test to every spec run of its
// project in the window (about 600k rows for 30 days), which takes seconds.
// The cache keeps each requested window's finished response, JSON and gzip,
// and rebuilds it in the background when anything is written (a test run
// ingested, a video attached, the registry imported, metadata edited) and
// when it is older than coverageMaxAge, so a read never waits for the join
// once the window has been built.
//
// A read right after a write may see the previous build for the few seconds
// the rebuild takes; ?fresh=1 builds synchronously.

const (
	coverageMaxAge     = 5 * time.Minute  // rebuild at least this often (the window slides)
	coverageSettle     = 2 * time.Second  // let a burst of writes settle before rebuilding
	coverageMinGap     = 10 * time.Second // never rebuild a window more often than this
	coverageForgetIdle = 30 * time.Minute // stop rebuilding a window nobody has read for this long
)

type coverageEntry struct {
	json, gz []byte
	gen      uint64    // write generation the build started from
	builtAt  time.Time // when the build started
	readAt   time.Time
}

type coverageCache struct {
	mu       sync.Mutex
	entries  map[string]*coverageEntry
	building map[string]chan struct{} // closed when that key's build finishes
	gen      atomic.Uint64
	wake     chan struct{}
}

// EnableCache makes GET /requirements/coverage serve built responses and
// starts the rebuild worker. Without it every read computes (tests do that).
func (h *CoverageHandler) EnableCache(ctx context.Context) {
	h.cache = &coverageCache{entries: map[string]*coverageEntry{}, building: map[string]chan struct{}{}, wake: make(chan struct{}, 1)}
	go h.cacheWorker(ctx)
	// Warm the window the page reads.
	go h.buildCoverage(DefaultCoverageBranch, DefaultCoverageDays) //nolint:errcheck
}

// InvalidateOnWrite is router middleware: any request that can write (not
// GET, HEAD or OPTIONS) marks every built window out of date once it has
// been handled.
func (h *CoverageHandler) InvalidateOnWrite() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			return
		}
		h.Invalidate()
	}
}

// Invalidate marks every built window out of date and schedules a rebuild.
func (h *CoverageHandler) Invalidate() {
	if h.cache == nil {
		return
	}
	h.cache.gen.Add(1)
	select {
	case h.cache.wake <- struct{}{}:
	default:
	}
}

func coverageKey(branch string, days int) string { return branch + "\x00" + strconv.Itoa(days) }

func (h *CoverageHandler) cacheWorker(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	var last time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.cache.wake:
			time.Sleep(coverageSettle)
			if d := time.Until(last.Add(coverageMinGap)); d > 0 {
				time.Sleep(d)
			}
		case <-tick.C:
		}
		last = time.Now()
		h.rebuildStale()
	}
}

// rebuildStale rebuilds every window read recently that is out of date.
func (h *CoverageHandler) rebuildStale() {
	gen := h.cache.gen.Load()
	type win struct {
		branch string
		days   int
	}
	var todo []win
	h.cache.mu.Lock()
	for k, e := range h.cache.entries {
		if time.Since(e.readAt) > coverageForgetIdle {
			delete(h.cache.entries, k)
			continue
		}
		if e.gen != gen || time.Since(e.builtAt) > coverageMaxAge {
			i := strings.IndexByte(k, 0)
			d, _ := strconv.Atoi(k[i+1:])
			todo = append(todo, win{k[:i], d})
		}
	}
	h.cache.mu.Unlock()
	for _, w := range todo {
		if _, err := h.buildCoverage(w.branch, w.days); err != nil {
			h.logger.WithError(err).Error("coverage rebuild failed")
		}
	}
}

// buildCoverage computes one window and stores it. Concurrent builds of the
// same window share one computation.
func (h *CoverageHandler) buildCoverage(branch string, days int) (*coverageEntry, error) {
	key := coverageKey(branch, days)
	h.cache.mu.Lock()
	if ch, ok := h.cache.building[key]; ok {
		h.cache.mu.Unlock()
		<-ch
		h.cache.mu.Lock()
		e := h.cache.entries[key]
		h.cache.mu.Unlock()
		if e == nil {
			return nil, errCoverageBuild
		}
		return e, nil
	}
	ch := make(chan struct{})
	h.cache.building[key] = ch
	h.cache.mu.Unlock()
	defer func() {
		h.cache.mu.Lock()
		delete(h.cache.building, key)
		h.cache.mu.Unlock()
		close(ch)
	}()

	gen, started := h.cache.gen.Load(), time.Now()
	body, err := h.coverageJSON(branch, days)
	if err != nil {
		return nil, err
	}
	var gz bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	zw.Write(body) //nolint:errcheck
	zw.Close()
	e := &coverageEntry{json: body, gz: gz.Bytes(), gen: gen, builtAt: started, readAt: started}
	h.cache.mu.Lock()
	if old := h.cache.entries[key]; old != nil && old.readAt.After(e.readAt) {
		e.readAt = old.readAt
	}
	h.cache.entries[key] = e
	h.cache.mu.Unlock()
	return e, nil
}

var errCoverageBuild = errors.New("coverage build failed")

// serveCached answers from the cache, building the window first when it has
// never been built (or when ?fresh=1). It reports false when the cache is off.
func (h *CoverageHandler) serveCached(c *gin.Context, branch string, days int) bool {
	if h.cache == nil {
		return false
	}
	key := coverageKey(branch, days)
	h.cache.mu.Lock()
	e := h.cache.entries[key]
	if e != nil {
		e.readAt = time.Now()
	}
	h.cache.mu.Unlock()
	if e == nil || c.Query("fresh") == "1" {
		var err error
		if e, err = h.buildCoverage(branch, days); err != nil {
			h.logger.WithError(err).Error("coverage lookup failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return true
		}
	} else if e.gen != h.cache.gen.Load() || time.Since(e.builtAt) > coverageMaxAge {
		// Out of date: answer now, rebuild behind.
		select {
		case h.cache.wake <- struct{}{}:
		default:
		}
	}
	c.Header("Vary", "Accept-Encoding")
	c.Header("X-Coverage-Built-At", e.builtAt.UTC().Format(time.RFC3339))
	if strings.Contains(c.GetHeader("Accept-Encoding"), "gzip") {
		c.Header("Content-Encoding", "gzip")
		c.Data(http.StatusOK, "application/json; charset=utf-8", e.gz)
	} else {
		c.Data(http.StatusOK, "application/json; charset=utf-8", e.json)
	}
	return true
}

// coverageJSON is the GET /requirements/coverage body for one window.
func (h *CoverageHandler) coverageJSON(branch string, days int) ([]byte, error) {
	body, err := h.coverageBody(branch, days)
	if err != nil {
		return nil, err
	}
	return json.Marshal(body)
}
