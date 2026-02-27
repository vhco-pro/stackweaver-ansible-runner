// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package routes

import (
	"os"

	"github.com/gin-gonic/gin"
	"github.com/iac-platform/backend/internal/api/handlers"
	"github.com/iac-platform/backend/internal/api/middleware"
	v2handlers "github.com/iac-platform/backend/internal/api/v2/handlers"
	v2routes "github.com/iac-platform/backend/internal/api/v2/routes"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/activity"
	"github.com/iac-platform/backend/internal/services/apikey"
	"github.com/iac-platform/backend/internal/services/auth"
	"github.com/iac-platform/backend/internal/services/profile"
	"github.com/iac-platform/backend/internal/services/sessions"
	"github.com/iac-platform/backend/internal/services/totp"
	"github.com/iac-platform/backend/internal/services/vcs"
	"gorm.io/gorm"
)

func SetupRoutes(
	db *gorm.DB,
	authService *auth.Service,
	totpService *totp.Service,
	profileService *profile.Service,
	sessionsService *sessions.Service,
	apiKeyService *apikey.Service,
	githubAppManager *vcs.GitHubAppManager,
) *gin.Engine {
	r := gin.Default()

	// Middleware
	r.Use(middleware.CORSMiddleware())
	r.Use(middleware.NewIPRateLimiter(100, 200).Middleware())

	// Health check (supports both GET and HEAD for healthchecks)
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})
	r.HEAD("/health", func(c *gin.Context) {
		c.Status(200)
	})

	// Terraform Service Discovery (public endpoint - no auth required)
	// GET /.well-known/terraform.json
	r.GET("/.well-known/terraform.json", v2handlers.HandleServiceDiscovery)

	// ==========================================
	// Webhook Routes (no auth - uses signature validation)
	// ==========================================
	webhookSecret := os.Getenv("GITHUB_WEBHOOK_SECRET")
	playbookRepo := repository.NewAnsiblePlaybookRepository(db)
	vcsRepo := repository.NewVCSConnectionRepository(db)
	githubWebhookHandler := handlers.NewGitHubWebhookHandler(playbookRepo, vcsRepo, nil, webhookSecret)

	webhooks := r.Group("/api/v2/webhooks")
	{
		webhooks.POST("/github", githubWebhookHandler.HandleWebhook)
	}

	// Setup v2 API routes
	v2routes.SetupV2Routes(r, db, authService, githubAppManager)

	// API v1
	v1 := r.Group("/api/v1")
	v1.Use(middleware.AuthMiddleware(authService))

	// Settings endpoints (v2)
	settings := r.Group("/api/v2/settings")
	settings.Use(middleware.AuthMiddleware(authService))
	{
		// 2FA Settings (only if TOTP service is available)
		if totpService != nil {
			totpHandler := handlers.NewTOTPHandler(totpService, authService)
			twoFA := settings.Group("/2fa")
			{
				twoFA.GET("/status", totpHandler.GetTOTPStatus)
				twoFA.POST("/start", totpHandler.StartTOTPRegistration)
				twoFA.POST("/verify", totpHandler.VerifyTOTP)
				twoFA.DELETE("", totpHandler.RemoveTOTP)
			}
			settings.POST("/password", totpHandler.ChangePassword)
			settings.GET("/mfa-devices", totpHandler.ListMFADevices)
		}

		// Profile Settings
		userRepo := repository.NewUserRepository(db)
		profileHandler := handlers.NewProfileHandler(profileService, authService, userRepo)
		settings.GET("/profile", profileHandler.GetProfile)
		settings.PATCH("/profile", profileHandler.UpdateProfile)

		// Sessions Settings (only if sessions service is available)
		if sessionsService != nil {
			sessionsHandler := handlers.NewSessionsHandler(sessionsService, authService)
			settings.GET("/sessions", sessionsHandler.ListSessions)
			settings.DELETE("/sessions/:sessionId", sessionsHandler.RevokeSession)
		}

		// API Keys Settings
		if apiKeyService != nil {
			auditLogRepo := repository.NewAuditLogRepository(db)
			activityService := activity.NewService(auditLogRepo)
			apiKeyHandler := handlers.NewAPIKeyHandler(apiKeyService, authService, activityService)
			settings.GET("/api-keys", apiKeyHandler.ListAPIKeys)
			settings.POST("/api-keys", apiKeyHandler.CreateAPIKey)
			settings.DELETE("/api-keys/:id", apiKeyHandler.DeleteAPIKey)
		}
	}

	return r
}
