// Package middleware provides request middleware for the Fern Platform
package middleware

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/guidewire-oss/fern-platform/pkg/logging"
)

// RequestIDMiddleware adds a unique request ID to each request
func RequestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := c.GetHeader("X-Request-ID")
		if requestID == "" {
			requestID = uuid.New().String()
		}

		c.Set("request_id", requestID)
		c.Header("X-Request-ID", requestID)
		c.Next()
	}
}

// LoggingMiddleware logs HTTP requests
func LoggingMiddleware(logger *logging.Logger) gin.HandlerFunc {
	return gin.LoggerWithConfig(gin.LoggerConfig{
		Formatter: func(param gin.LogFormatterParams) string {
			// Skip logging for health check endpoints
			if param.Path == "/health" || param.Path == "/api/v1/health" {
				return ""
			}

			entry := logger.WithFields(map[string]interface{}{
				"timestamp":     param.TimeStamp.Format(time.RFC3339),
				"status":        param.StatusCode,
				"latency":       param.Latency.String(),
				"client_ip":     param.ClientIP,
				"method":        param.Method,
				"path":          param.Path,
				"user_agent":    param.Request.UserAgent(),
				"request_id":    param.Keys["request_id"],
				"response_size": param.BodySize,
			})

			if param.ErrorMessage != "" {
				entry = entry.WithField("error", param.ErrorMessage)
			}

			if param.StatusCode >= 500 {
				entry.Error("HTTP request completed with server error")
			} else if param.StatusCode >= 400 {
				entry.Warn("HTTP request completed with client error")
			} else {
				entry.Info("HTTP request completed")
			}

			return ""
		},
		Output: logger.Logger.Out,
	})
}

// RecoveryMiddleware recovers from panics and logs them
func RecoveryMiddleware(logger *logging.Logger) gin.HandlerFunc {
	return gin.CustomRecovery(func(c *gin.Context, recovered interface{}) {
		requestID := c.GetString("request_id")
		logger.WithRequest(requestID, c.Request.Method, c.Request.URL.Path).
			WithField("panic", recovered).
			Error("Panic recovered")

		c.JSON(500, gin.H{
			"error":      "Internal server error",
			"request_id": requestID,
		})
	})
}

// SecurityHeadersMiddleware adds security headers to responses
func SecurityHeadersMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("X-XSS-Protection", "1; mode=block")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Header("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline' https://unpkg.com https://d3js.org; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com https://cdnjs.cloudflare.com; img-src 'self' data: https:; font-src 'self' https://fonts.gstatic.com; media-src 'self' https://cdn.dev.hiolly.com; connect-src 'self'")
		c.Next()
	}
}

// HealthCheckMiddleware provides a health check endpoint
func HealthCheckMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.URL.Path == "/health" && c.Request.Method == "GET" {
			c.JSON(200, gin.H{
				"status":    "healthy",
				"timestamp": time.Now().UTC().Format(time.RFC3339),
				"service":   "fern-platform",
			})
			c.Abort()
			return
		}
		c.Next()
	}
}

// RateLimitMiddleware provides basic rate limiting (simplified implementation)
func RateLimitMiddleware() gin.HandlerFunc {
	// In a production environment, you would use a proper rate limiter like redis-based
	// This is a simplified in-memory implementation for basic protection
	return func(c *gin.Context) {
		// For now, just pass through - implement proper rate limiting as needed
		c.Next()
	}
}
