// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/michielvha/logger"
	"github.com/zitadel/oidc/v3/pkg/oidc"
)

// APIKeyVerifier interface for API key verification
type APIKeyVerifier interface {
	VerifyAPIKey(key string) (*models.APIKey, error)
	UpdateLastUsed(id uuid.UUID) error
}

// TeamSyncer interface for automatic team assignment based on SSO group claims
type TeamSyncer interface {
	IsEnabled() bool
	SyncUserTeams(ctx context.Context, userID uuid.UUID, ssoGroups []string) error
}

type Service struct {
	userRepo      *repository.UserRepository
	tfeTokenRepo  *repository.TFETokenRepository
	apiKeyService APIKeyVerifier
	teamSyncer    TeamSyncer
	verifier      *ZitadelVerifier
	issuer        string
	clientID      string
}

func NewService(userRepo *repository.UserRepository, tfeTokenRepo *repository.TFETokenRepository) *Service {
	return &Service{
		userRepo:     userRepo,
		tfeTokenRepo: tfeTokenRepo,
	}
}

// SetAPIKeyService sets the API key service for authentication
func (s *Service) SetAPIKeyService(apiKeyService APIKeyVerifier) {
	s.apiKeyService = apiKeyService
}

// SetTeamSyncer sets the team sync service for automatic SSO team assignment
func (s *Service) SetTeamSyncer(syncer TeamSyncer) {
	s.teamSyncer = syncer
}

// InitializeZitadel initializes the Zitadel verifier with configuration.
// internalAddr (e.g. "localhost:8080") is used to fetch JWKS keys internally
// when the issuer is an external domain, avoiding public internet round-trips.
func (s *Service) InitializeZitadel(issuer, clientID, clientSecret, internalAddr string) error {
	verifier, err := NewZitadelVerifier(issuer, clientID, clientSecret, internalAddr)
	if err != nil {
		return fmt.Errorf("failed to create Zitadel verifier: %w", err)
	}
	s.verifier = verifier
	s.issuer = issuer
	s.clientID = clientID
	return nil
}

func (s *Service) GetUserFromContext(c *gin.Context) (*models.User, error) {
	userID, exists := c.Get("user_id")
	if !exists {
		return nil, errors.New("user not authenticated")
	}

	id, ok := userID.(uuid.UUID)
	if !ok {
		return nil, errors.New("invalid user ID in context")
	}

	user, err := s.userRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("failed to get user: %w", err)
	}

	return user, nil
}

// GetUserFromToken authenticates a token and returns the user
// Supports TFE tokens (tfe- prefix) and JWT tokens
func (s *Service) GetUserFromToken(tokenString string) (*models.User, error) {
	// Try TFE token first (if it starts with "tfe-")
	if strings.HasPrefix(tokenString, "tfe-") {
		// Try TFE token lookup
		tfeToken, err := s.tfeTokenRepo.GetByToken(tokenString)
		if err == nil {
			// Update last used timestamp
			_ = s.tfeTokenRepo.UpdateLastUsed(tfeToken.ID)

			// Get user
			user, err := s.userRepo.GetByID(tfeToken.UserID)
			if err == nil {
				return user, nil
			}
		}

		// If TFE token lookup fails, try API key (Terraform Cloud compatible)
		if s.apiKeyService != nil {
			apiKey, err := s.apiKeyService.VerifyAPIKey(tokenString)
			if err == nil && apiKey != nil {
				// Update last used timestamp
				_ = s.apiKeyService.UpdateLastUsed(apiKey.ID)

				// Get user
				user, err := s.userRepo.GetByID(apiKey.UserID)
				if err == nil {
					return user, nil
				}
			}
		}
	}

	// Try JWT token (Zitadel)
	if s.verifier == nil {
		return nil, errors.New("authentication service not initialized")
	}

	// Verify JWT token (need context for verification)
	ctx := context.Background()
	claims, claimsMap, err := s.verifier.VerifyToken(ctx, tokenString)
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}

	// Extract user info from claims (using raw claims map for custom Zitadel fields)
	// If email is missing, fallback to UserInfo endpoint (standard OIDC)
	userInfo := ExtractUserInfo(ctx, claims, claimsMap, s.issuer, tokenString, s.verifier.httpClient)

	// Get or create user in database by Zitadel subject
	user, err := s.userRepo.GetOrCreateByZitadelSubject(
		userInfo.Subject,
		userInfo.Email,
		userInfo.Name,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get or create user: %w", err)
	}

	return user, nil
}

func (s *Service) AuthenticateMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Skip authentication for OPTIONS requests (CORS preflight)
		if c.Request.Method == "OPTIONS" {
			c.Next()
			return
		}

		// TFE-compatible: For logs endpoint, allow token in query parameter OR Authorization header
		// According to TFE docs, all endpoints use Authorization header, but Terraform CLI may call
		// log-read-url without header if token is in URL. Support both methods for compatibility.
		if strings.HasPrefix(c.Request.URL.Path, "/api/v2/runs/") && strings.HasSuffix(c.Request.URL.Path, "/logs") {
			// Try token in query parameter first (for log-read-url with embedded token)
			tokenFromQuery := c.Query("token")
			if tokenFromQuery != "" {
				// Authenticate using token from query parameter
				user, err := s.GetUserFromToken(tokenFromQuery)
				if err == nil {
					c.Set("user_id", user.ID)
					c.Set("user_email", user.Email)
					c.Set("user_name", user.Name)
					c.Set("auth_method", "query_token")
					c.Next()
					return
				}
			}
			// TFE-compatible: If no token in query and no Authorization header, allow the request to proceed
			// The logs handler will validate the run exists (security check)
			// Terraform CLI calls log-read-url without auth, relying on the URL being pre-authenticated
			// Since we can't pre-authenticate the URL, we allow the request and validate in the handler
			authHeader := c.GetHeader("Authorization")
			if authHeader == "" {
				// No auth provided - allow request to proceed to handler for validation
				// The handler will check if the run exists and return appropriate response
				c.Next()
				return
			}
			// If Authorization header is present, continue with normal auth flow below
		}

		// Extract token from Authorization header (standard TFE authentication)
		authHeader := c.GetHeader("Authorization")
		logger.Debugf("auth: authorization header present: %v", authHeader != "")
		if authHeader == "" {
			logger.Debugf("auth: missing authorization header for path: %s", c.Request.URL.Path)
			c.JSON(401, gin.H{
				"errors": []gin.H{
					{
						"status": "401",
						"title":  "Unauthorized",
						"detail": "missing authorization header",
					},
				},
			})
			c.Abort()
			return
		}

		// Check for Bearer token
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || parts[0] != "Bearer" {
			c.JSON(401, gin.H{
				"errors": []gin.H{
					{
						"status": "401",
						"title":  "Unauthorized",
						"detail": "invalid authorization header format",
					},
				},
			})
			c.Abort()
			return
		}

		tokenString := parts[1]

		// Debug logging for authentication attempts
		tokenPreview := tokenString
		if len(tokenString) > 10 {
			tokenPreview = tokenString[:10]
		}
		logger.Debugf("auth: received token (first 10 chars): %s...", tokenPreview)
		logger.Debugf("auth: request path: %s", c.Request.URL.Path)
		logger.Debugf("auth: request method: %s", c.Request.Method)

		// Try API key or TFE token first (if it starts with "tfe-")
		if strings.HasPrefix(tokenString, "tfe-") {
			logger.Debug("auth: token starts with 'tfe-', attempting TFE token lookup")
			// First try TFE token (legacy)
			tfeToken, err := s.tfeTokenRepo.GetByToken(tokenString)
			if err == nil {
				logger.Debugf("auth: TFE token lookup successful, user_id: %s", tfeToken.UserID)
				// Update last used timestamp
				_ = s.tfeTokenRepo.UpdateLastUsed(tfeToken.ID)

				// Get user
				user, err := s.userRepo.GetByID(tfeToken.UserID)
				if err == nil {
					// Store user in context
					c.Set("user_id", user.ID)
					c.Set("user_email", user.Email)
					c.Set("user_name", user.Name)
					c.Set("auth_method", "tfe_token")
					c.Next()
					return
				}
			}

			// If TFE token lookup fails, try API key (Terraform Cloud compatible)
			if s.apiKeyService != nil {
				apiKey, err := s.apiKeyService.VerifyAPIKey(tokenString)
				if err == nil && apiKey != nil {
					// Update last used timestamp
					_ = s.apiKeyService.UpdateLastUsed(apiKey.ID)

					// Get user
					user, err := s.userRepo.GetByID(apiKey.UserID)
					if err == nil {
						// Store user in context
						c.Set("user_id", user.ID)
						c.Set("user_email", user.Email)
						c.Set("user_name", user.Name)
						c.Set("auth_method", "api_key")
						// Store API key details for runner registration and scope checks
						c.Set("api_key_id", apiKey.ID)
						c.Set("api_key_scopes", []string(apiKey.Scopes))
						c.Next()
						return
					}
				}
			}
			// If both fail, continue to JWT verification
			if err != nil {
				logger.Debugf("auth: TFE token lookup failed: %v", err)
			}
		} else {
			logger.Debug("auth: token does not start with 'tfe-', attempting JWT verification")
		}

		// Try JWT token (Zitadel)
		if s.verifier == nil {
			c.JSON(500, gin.H{
				"errors": []gin.H{
					{
						"status": "500",
						"title":  "Internal Server Error",
						"detail": "authentication service not initialized",
					},
				},
			})
			c.Abort()
			return
		}

		// Verify JWT token
		claims, claimsMap, err := s.verifier.VerifyToken(c.Request.Context(), tokenString)
		if err != nil {
			// Log the actual error for debugging (but don't expose it to client for security)
			logger.Warnf("auth: token verification failed: %v", err)
			c.JSON(401, gin.H{
				"errors": []gin.H{
					{
						"status": "401",
						"title":  "Unauthorized",
						"detail": "invalid token",
					},
				},
			})
			c.Abort()
			return
		}

		// Extract user info from claims (using raw claims map for custom Zitadel fields)
		// If email is missing, fallback to UserInfo endpoint (standard OIDC)
		userInfo := ExtractUserInfo(c.Request.Context(), claims, claimsMap, s.issuer, tokenString, s.verifier.httpClient)

		logger.Infof("Auth: Subject=%s Email=%s Groups=%v", userInfo.Subject, userInfo.Email, userInfo.Groups)

		// Get or create user in database by Zitadel subject
		// This ensures users are automatically created on first authentication
		user, err := s.userRepo.GetOrCreateByZitadelSubject(
			userInfo.Subject,
			userInfo.Email,
			userInfo.Name,
		)
		if err != nil {
			c.JSON(500, gin.H{
				"errors": []gin.H{
					{
						"status": "500",
						"title":  "Internal Server Error",
						"detail": "failed to get or create user",
					},
				},
			})
			c.Abort()
			return
		}

		// Store local user UUID in context (not Zitadel subject)
		c.Set("user_id", user.ID)
		c.Set("user_email", user.Email)
		c.Set("user_name", user.Name)
		c.Set("user_subject", userInfo.Subject)
		c.Set("token_claims", claims)
		c.Set("auth_method", "jwt")

		// Store SSO groups in context for downstream team sync
		if len(userInfo.Groups) > 0 {
			c.Set("sso_groups", userInfo.Groups)

			// Trigger automatic team sync based on SSO group claims
			if s.teamSyncer != nil && s.teamSyncer.IsEnabled() {
				if err := s.teamSyncer.SyncUserTeams(c.Request.Context(), user.ID, userInfo.Groups); err != nil {
					logger.Errorf("TeamSync failed for user %s: %v", user.ID, err)
					// Don't block authentication on team sync failure
				}
			}
		}

		// Continue to next handler
		c.Next()
	}
}

func (s *Service) GetUserID(ctx context.Context) (uuid.UUID, error) {
	// Try to get from gin context if available
	if c, ok := ctx.(*gin.Context); ok {
		userID, exists := c.Get("user_id")
		if exists {
			if idStr, ok := userID.(string); ok {
				// Try to parse as UUID
				if id, err := uuid.Parse(idStr); err == nil {
					return id, nil
				}
				// If not a UUID, return error
				return uuid.Nil, fmt.Errorf("user_id is not a valid UUID: %s", idStr)
			}
		}
	}
	return uuid.Nil, errors.New("user not authenticated")
}

// GetUserSubject returns the Zitadel subject (user ID) from context
func (s *Service) GetUserSubject(c *gin.Context) (string, error) {
	subject, exists := c.Get("user_subject")
	if !exists {
		return "", errors.New("user not authenticated")
	}
	subjectStr, ok := subject.(string)
	if !ok {
		return "", errors.New("invalid subject type")
	}
	return subjectStr, nil
}

// GetTokenClaims returns the token claims from context
func (s *Service) GetTokenClaims(c *gin.Context) (*oidc.AccessTokenClaims, error) {
	claims, exists := c.Get("token_claims")
	if !exists {
		return nil, errors.New("token claims not found")
	}
	tokenClaims, ok := claims.(*oidc.AccessTokenClaims)
	if !ok {
		return nil, errors.New("invalid token claims type")
	}
	return tokenClaims, nil
}
