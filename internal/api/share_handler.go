// Package api provides domain-based REST API handlers
package api

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/guidewire-oss/fern-platform/pkg/database"
	"github.com/guidewire-oss/fern-platform/pkg/logging"
)

const (
	shareCodeLength   = 10
	defaultShareTTL   = 168 * time.Hour      // 7 days
	maxShareTTL       = 365 * 24 * time.Hour // 1 year
	maxSharePathBytes = 2048

	// 64 URL-safe characters so shareCodeAlphabet[b&63] carries no modulo bias.
	shareCodeAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_"
)

// ShareHandler issues and resolves short share codes ("presigned URLs") for
// UI paths, e.g. /web/#/test-runs/{runId}/{suite}/{spec}. The code itself is
// the credential: anyone holding an unexpired code can follow the redirect
// and, once auth is re-enabled, read through the share bypass in the auth
// middleware (see internal/domains/auth/interfaces/middleware_adapter.go).
type ShareHandler struct {
	*BaseHandler
	db *gorm.DB
}

// NewShareHandler creates a new share link handler
func NewShareHandler(db *gorm.DB, logger *logging.Logger) *ShareHandler {
	return &ShareHandler{
		BaseHandler: NewBaseHandler(logger),
		db:          db,
	}
}

// RegisterRoutes registers share-link routes. Resolution is public — the code
// is the credential; creation sits on the authenticated user group like the
// other user-level API routes.
func (h *ShareHandler) RegisterRoutes(router *gin.Engine, userGroup *gin.RouterGroup) {
	router.GET("/s/:code", h.resolveShareLink)
	userGroup.POST("/share", h.createShareLink)
}

type createShareLinkRequest struct {
	Path     string `json:"path" binding:"required"`
	TTLHours int    `json:"ttl_hours"`
}

// createShareLink handles POST /api/v1/share
func (h *ShareHandler) createShareLink(c *gin.Context) {
	var req createShareLinkRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.respondWithError(c, http.StatusBadRequest, "Invalid request: "+err.Error())
		return
	}

	// Only same-origin absolute paths may be shared. Rejecting a second
	// leading "/" or "\" closes the protocol-relative open-redirect hole
	// ("//evil.com", "/\evil.com").
	if !strings.HasPrefix(req.Path, "/") || len(req.Path) > maxSharePathBytes ||
		strings.HasPrefix(req.Path, "//") || strings.HasPrefix(req.Path, "/\\") ||
		strings.ContainsAny(req.Path, "\r\n") {
		h.respondWithError(c, http.StatusBadRequest, "path must be a same-origin absolute path")
		return
	}

	ttl := defaultShareTTL
	if req.TTLHours > 0 {
		ttl = time.Duration(req.TTLHours) * time.Hour
	}
	if ttl > maxShareTTL {
		ttl = maxShareTTL
	}

	createdBy := c.GetString("user_id")

	// Retry a couple of times on the (astronomically unlikely) code collision.
	var link database.ShareLink
	for attempt := 0; ; attempt++ {
		code, err := generateShareCode(shareCodeLength)
		if err != nil {
			h.logger.WithError(err).Error("Failed to generate share code")
			h.respondWithError(c, http.StatusInternalServerError, "Failed to create share link")
			return
		}
		link = database.ShareLink{
			Code:      code,
			Path:      req.Path,
			CreatedBy: createdBy,
			ExpiresAt: time.Now().UTC().Add(ttl),
		}
		err = h.db.Create(&link).Error
		if err == nil {
			break
		}
		if attempt >= 2 {
			h.logger.WithError(err).Error("Failed to persist share link")
			h.respondWithError(c, http.StatusInternalServerError, "Failed to create share link")
			return
		}
	}

	h.respondWithJSON(c, http.StatusCreated, gin.H{
		"code":       link.Code,
		"url":        fmt.Sprintf("%s/s/%s", requestBaseURL(c), link.Code),
		"expires_at": link.ExpiresAt.Format(time.RFC3339),
	})
}

// resolveShareLink handles GET /s/:code — 404 unknown, 410 expired, else 302
// to the stored path with ?share=<code> attached ahead of any #fragment so
// the SPA request carries the code once auth is re-enabled.
func (h *ShareHandler) resolveShareLink(c *gin.Context) {
	code := c.Param("code")

	var link database.ShareLink
	err := h.db.Where("code = ?", code).First(&link).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		h.respondWithError(c, http.StatusNotFound, "Unknown share code")
		return
	}
	if err != nil {
		h.logger.WithError(err).Error("Failed to look up share link")
		h.respondWithError(c, http.StatusInternalServerError, "Failed to resolve share link")
		return
	}
	if time.Now().After(link.ExpiresAt) {
		h.respondWithError(c, http.StatusGone, "Share link expired")
		return
	}

	c.Redirect(http.StatusFound, buildShareRedirectTarget(link.Path, link.Code))
}

// ValidateShareCode reports whether a share code exists and is unexpired.
// It implements the auth middleware's ShareCodeValidator so shared links keep
// working when auth is enabled.
func (h *ShareHandler) ValidateShareCode(ctx context.Context, code string) bool {
	if code == "" || len(code) > 64 {
		return false
	}
	var link database.ShareLink
	if err := h.db.WithContext(ctx).Where("code = ?", code).First(&link).Error; err != nil {
		return false
	}
	return time.Now().Before(link.ExpiresAt)
}

// generateShareCode returns an n-character URL-safe random code.
func generateShareCode(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = shareCodeAlphabet[b[i]&63]
	}
	return string(b), nil
}

// buildShareRedirectTarget appends share=<code> to the query of a stored path,
// keeping it before the #fragment (the fragment never reaches the server, so
// the code must ride the query string for the share auth bypass to see it).
func buildShareRedirectTarget(path, code string) string {
	base, frag, hasFrag := strings.Cut(path, "#")
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	target := base + sep + "share=" + code
	if hasFrag {
		target += "#" + frag
	}
	return target
}

// requestBaseURL reconstructs the externally visible scheme://host for this
// request (nginx forwards Host and X-Forwarded-Proto).
func requestBaseURL(c *gin.Context) string {
	scheme := "http"
	if c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + c.Request.Host
}
