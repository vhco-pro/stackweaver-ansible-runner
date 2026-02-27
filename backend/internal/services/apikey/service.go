// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package apikey

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"golang.org/x/crypto/bcrypt"
)

type Service struct {
	apiKeyRepo  *repository.APIKeyRepository
	orgRepo     *repository.OrganizationRepository
	projectRepo *repository.ProjectRepository
	teamRepo    *repository.TeamRepository
}

func NewService(apiKeyRepo *repository.APIKeyRepository, orgRepo *repository.OrganizationRepository, projectRepo *repository.ProjectRepository, teamRepo *repository.TeamRepository) *Service {
	return &Service{
		apiKeyRepo:  apiKeyRepo,
		orgRepo:     orgRepo,
		projectRepo: projectRepo,
		teamRepo:    teamRepo,
	}
}

// GenerateAPIKey generates a new API key string
// Format: tfe-<random_base64> (Terraform Cloud compatible)
func GenerateAPIKey() (string, error) {
	// Generate 32 random bytes (256 bits)
	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}

	// Encode to base64 URL-safe (no padding) - matching Terraform Cloud format
	keySuffix := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(randomBytes)

	// Format: tfe-<suffix> (Terraform Cloud compatible)
	return fmt.Sprintf("tfe-%s", keySuffix), nil
}

// HashKey hashes an API key using bcrypt
func HashKey(key string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(key), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("failed to hash API key: %w", err)
	}
	return string(hash), nil
}

// VerifyKey verifies an API key against its hash
func VerifyKey(key, hash string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(key))
	return err == nil
}

// GetKeyPrefix extracts the first 12 characters of a key for display
// Format: tfe-<first 8 chars of suffix>
func GetKeyPrefix(key string) string {
	// Key format is "tfe-<suffix>"
	// We want to show "tfe-<first 8 chars>"
	if len(key) <= 12 {
		return key
	}
	// Show first 12 characters (tfe- + first 8 chars of suffix)
	return key[:12]
}

// CreateAPIKey creates a new API key for a user
func (s *Service) CreateAPIKey(userID uuid.UUID, name string, scopes []string, expiresAt *time.Time) (*models.APIKey, string, error) {
	// Generate the API key
	key, err := GenerateAPIKey()
	if err != nil {
		return nil, "", fmt.Errorf("failed to generate API key: %w", err)
	}

	// Hash the key
	keyHash, err := HashKey(key)
	if err != nil {
		return nil, "", fmt.Errorf("failed to hash API key: %w", err)
	}

	// If no scopes provided, default to full access (empty array means all scopes)
	// This maintains backward compatibility
	if scopes == nil {
		scopes = []string{}
	}

	// Validate and parse scopes
	parsedScopes, err := ParseScopes(scopes)
	if err != nil {
		return nil, "", fmt.Errorf("invalid scopes: %w", err)
	}

	// Validate organization, project, and team access
	var orgID *uuid.UUID
	var projectID *uuid.UUID
	var teamID *uuid.UUID

	for _, scope := range parsedScopes {
		if scope.Type == "team" && scope.ResourceID != nil {
			// Validate that the team exists
			team, err := s.teamRepo.GetByID(*scope.ResourceID)
			if err != nil {
				return nil, "", fmt.Errorf("team not found: %w", err)
			}

			// Check if user has access to the team's organization (team-based)
			inOrg, err := s.orgRepo.UserInOrg(userID, team.OrganizationID)
			if err != nil || !inOrg {
				return nil, "", fmt.Errorf("user is not a member of the team's organization")
			}

			// If multiple team scopes, they must all be for the same team
			if teamID != nil && *teamID != *scope.ResourceID {
				return nil, "", fmt.Errorf("API key cannot be scoped to multiple teams")
			}
			teamID = scope.ResourceID

			// If team is scoped, set org ID from team
			if orgID == nil {
				orgID = &team.OrganizationID
			} else if *orgID != team.OrganizationID {
				return nil, "", fmt.Errorf("team does not belong to the specified organization")
			}
		}

		if scope.Type == "org" && scope.ResourceID != nil {
			// Validate that the organization exists and user is a member
			org, err := s.orgRepo.GetByID(*scope.ResourceID)
			if err != nil {
				return nil, "", fmt.Errorf("organization not found: %w", err)
			}

			// Check if user has access to the organization (team-based)
			inOrg, err := s.orgRepo.UserInOrg(userID, *scope.ResourceID)
			if err != nil || !inOrg {
				return nil, "", fmt.Errorf("user is not a member of organization %s", scope.ResourceID.String())
			}
			_ = org // org exists

			// If multiple org scopes, they must all be for the same org
			if orgID != nil && *orgID != *scope.ResourceID {
				return nil, "", fmt.Errorf("API key cannot be scoped to multiple organizations")
			}
			orgID = scope.ResourceID
		}

		if scope.Type == "project" && scope.ResourceID != nil {
			// Validate that the project exists
			project, err := s.projectRepo.GetByID(*scope.ResourceID)
			if err != nil {
				return nil, "", fmt.Errorf("project not found: %w", err)
			}

			// Check if user has access to the project's organization (team-based)
			inOrg, err := s.orgRepo.UserInOrg(userID, project.OrganizationID)
			if err != nil || !inOrg {
				return nil, "", fmt.Errorf("user is not a member of the project's organization")
			}

			// If multiple project scopes, they must all be for the same project
			if projectID != nil && *projectID != *scope.ResourceID {
				return nil, "", fmt.Errorf("API key cannot be scoped to multiple projects")
			}
			projectID = scope.ResourceID

			// If project is scoped, set org ID from project
			if orgID == nil {
				orgID = &project.OrganizationID
			} else if *orgID != project.OrganizationID {
				return nil, "", fmt.Errorf("project does not belong to the specified organization")
			}
		}
	}

	// Create the API key record
	apiKey := &models.APIKey{
		UserID:         userID,
		Name:           name,
		KeyHash:        keyHash,
		KeyPrefix:      GetKeyPrefix(key),
		Scopes:         models.StringArray(scopes),
		OrganizationID: orgID,
		ProjectID:      projectID,
		ExpiresAt:      expiresAt,
	}

	if err := s.apiKeyRepo.Create(apiKey); err != nil {
		return nil, "", fmt.Errorf("failed to create API key: %w", err)
	}

	// Return the API key (plaintext) and the record
	// The plaintext key is only shown once during creation
	return apiKey, key, nil
}

// ListAPIKeys lists all API keys for a user
func (s *Service) ListAPIKeys(userID uuid.UUID) ([]*models.APIKey, error) {
	return s.apiKeyRepo.GetByUserID(userID)
}

// GetAPIKey gets a single API key by ID (for the owner)
func (s *Service) GetAPIKey(id uuid.UUID, userID uuid.UUID) (*models.APIKey, error) {
	apiKey, err := s.apiKeyRepo.GetByID(id)
	if err != nil {
		return nil, err
	}

	// Verify ownership
	if apiKey.UserID != userID {
		return nil, fmt.Errorf("API key not found")
	}

	return apiKey, nil
}

// DeleteAPIKey deletes an API key
func (s *Service) DeleteAPIKey(id uuid.UUID, userID uuid.UUID) error {
	// Verify ownership
	apiKey, err := s.apiKeyRepo.GetByID(id)
	if err != nil {
		return err
	}

	if apiKey.UserID != userID {
		return fmt.Errorf("API key not found")
	}

	return s.apiKeyRepo.Delete(id)
}

// VerifyAPIKey verifies an API key and returns the associated API key record
// Uses the key prefix for fast lookup, then verifies with bcrypt
func (s *Service) VerifyAPIKey(key string) (*models.APIKey, error) {
	// Extract prefix for fast lookup
	keyPrefix := GetKeyPrefix(key)

	// Get API keys with matching prefix (much faster than checking all keys)
	apiKeys, err := s.apiKeyRepo.GetByPrefix(keyPrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to lookup API keys: %w", err)
	}

	// Verify each key with matching prefix
	for _, apiKey := range apiKeys {
		// Verify the full key against the hash
		if VerifyKey(key, apiKey.KeyHash) {
			// Check if expired
			if apiKey.ExpiresAt != nil && apiKey.ExpiresAt.Before(time.Now()) {
				return nil, fmt.Errorf("API key has expired")
			}
			return apiKey, nil
		}
	}

	return nil, fmt.Errorf("invalid API key")
}

// UpdateLastUsed updates the last used timestamp for an API key
func (s *Service) UpdateLastUsed(id uuid.UUID) error {
	return s.apiKeyRepo.UpdateLastUsed(id)
}

// CheckPermission checks if an API key has permission for a specific resource
// Returns true if the key has the requested permission
func (s *Service) CheckPermission(apiKey *models.APIKey, resourceType string, resourceID *uuid.UUID, permission string) (bool, error) {
	checker, err := NewScopeChecker(apiKey.Scopes)
	if err != nil {
		return false, fmt.Errorf("failed to parse scopes: %w", err)
	}

	// Check if key is scoped to a specific organization/project
	if apiKey.OrganizationID != nil {
		// If resource is organization-scoped, verify it matches
		if resourceType == "org" && resourceID != nil {
			if *apiKey.OrganizationID != *resourceID {
				return false, nil
			}
		}
		// If resource is project-scoped, verify it belongs to the org
		if resourceType == "project" && resourceID != nil {
			project, err := s.projectRepo.GetByID(*resourceID)
			if err != nil {
				return false, nil
			}
			if project.OrganizationID != *apiKey.OrganizationID {
				return false, nil
			}
		}
	}

	if apiKey.ProjectID != nil {
		// If resource is project-scoped, verify it matches
		if resourceType == "project" && resourceID != nil {
			if *apiKey.ProjectID != *resourceID {
				return false, nil
			}
		}
	}

	return checker.HasPermission(resourceType, resourceID, permission), nil
}

// CheckOrgPermission checks if an API key has permission for an organization
func (s *Service) CheckOrgPermission(apiKey *models.APIKey, orgID uuid.UUID, permission string) (bool, error) {
	return s.CheckPermission(apiKey, "org", &orgID, permission)
}

// CheckProjectPermission checks if an API key has permission for a project
func (s *Service) CheckProjectPermission(apiKey *models.APIKey, projectID uuid.UUID, permission string) (bool, error) {
	return s.CheckPermission(apiKey, "project", &projectID, permission)
}

// CheckUserPermission checks if an API key has permission for user operations
func (s *Service) CheckUserPermission(apiKey *models.APIKey, permission string) (bool, error) {
	return s.CheckPermission(apiKey, "user", nil, permission)
}

// GetScopeChecker returns a scope checker for an API key
func (s *Service) GetScopeChecker(apiKey *models.APIKey) (*ScopeChecker, error) {
	return NewScopeChecker(apiKey.Scopes)
}
