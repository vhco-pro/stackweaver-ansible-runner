// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package middleware

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

func InputValidationMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Validate request size
		if c.Request.ContentLength > 10*1024*1024 { // 10MB limit
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request too large"})
			c.Abort()
			return
		}

		// Sanitize query parameters
		// Note: Query() returns a copy, so we need to reconstruct the URL
		query := c.Request.URL.Query()
		sanitized := url.Values{}
		for key, values := range query {
			for _, value := range values {
				// Remove potential SQL injection patterns
				sanitizedValue := strings.ReplaceAll(value, "'", "")
				sanitizedValue = strings.ReplaceAll(sanitizedValue, "\"", "")
				sanitizedValue = strings.ReplaceAll(sanitizedValue, ";", "")
				sanitizedValue = strings.ReplaceAll(sanitizedValue, "--", "")
				sanitized.Add(key, sanitizedValue)
			}
		}
		// Reconstruct URL with sanitized query parameters
		c.Request.URL.RawQuery = sanitized.Encode()

		c.Next()
	}
}
