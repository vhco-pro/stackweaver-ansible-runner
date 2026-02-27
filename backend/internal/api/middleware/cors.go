// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package middleware

import (
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

func CORSMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")

		// Allowed origins (for development, allow localhost on common ports)
		// Support both IPv4 (127.0.0.1) and IPv6 ([::1]) localhost
		allowedOrigins := []string{
			"http://localhost:5173", // Vite dev server
			"http://localhost:3000", // Alternative frontend port
			"http://localhost:5174", // Alternative Vite port
			"http://127.0.0.1:5173", // IPv4 localhost
			"http://127.0.0.1:3000", // IPv4 localhost
			"http://127.0.0.1:5174", // IPv4 localhost
			"http://[::1]:5173",     // IPv6 localhost
			"http://[::1]:3000",     // IPv6 localhost
			"http://[::1]:5174",     // IPv6 localhost
		}
		// Extra origins for Cloudflare Tunnel or other public frontend URLs (comma-separated)
		if extra := os.Getenv("CORS_EXTRA_ORIGINS"); extra != "" {
			for _, o := range strings.Split(extra, ",") {
				if o = strings.TrimSpace(o); o != "" {
					allowedOrigins = append(allowedOrigins, o)
				}
			}
		}

		// Check if origin is allowed
		allowed := false
		for _, allowedOrigin := range allowedOrigins {
			if origin == allowedOrigin {
				allowed = true
				break
			}
		}

		// Also allow if origin is empty (same-origin request) or if it's a localhost variant
		// This handles cases where the browser sends different localhost formats
		if !allowed && origin != "" {
			// Check if it's a localhost variant (any port)
			if (len(origin) > 16 && origin[:16] == "http://localhost") ||
				(len(origin) > 15 && origin[:15] == "http://127.0.0.1") ||
				(len(origin) > 10 && origin[:10] == "http://[::1]") {
				allowed = true
			}
		}

		// For OPTIONS requests (preflight), always set CORS headers if origin is present
		// This ensures the browser can complete the preflight check
		if c.Request.Method == "OPTIONS" {
			// Set CORS headers BEFORE checking if origin is allowed
			// This ensures preflight requests succeed even if origin check fails
			if origin != "" {
				// Use the origin (both allowed and development cases use the same origin)
				c.Writer.Header().Set("Access-Control-Allow-Origin", origin)
				c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
			}
			c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, accept, origin, Cache-Control, X-Requested-With")
			c.Writer.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS, GET, PUT, DELETE, PATCH")
			c.AbortWithStatus(204)
			return
		}

		// For actual requests, set CORS headers if origin is allowed or if it's a localhost variant
		if allowed || origin == "" {
			if origin != "" {
				c.Writer.Header().Set("Access-Control-Allow-Origin", origin)
				c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
			}
		}

		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, accept, origin, Cache-Control, X-Requested-With")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS, GET, PUT, DELETE, PATCH")

		c.Next()
	}
}
