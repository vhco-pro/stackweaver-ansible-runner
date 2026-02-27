// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// Package team_sync provides automatic team membership assignment based on SSO group claims.
// When users authenticate via an external IdP (Azure AD, Okta, AWS Cognito, etc.),
// their group memberships are forwarded as the "sso_groups" claim in the Zitadel JWT.
// This service maps those groups to StackWeaver teams via the sso_team_id field,
// automatically adding users to the correct teams on each login.
package team_sync

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/michielvha/logger"
	"gorm.io/gorm"
)

// Config holds the configuration for the TeamSyncService.
type Config struct {
	// Enabled controls whether automatic team sync is active.
	// Set via ENABLE_OIDC_TEAM_SYNC environment variable.
	Enabled bool

	// RemoveFromNonSSOTeams controls whether users are removed from SSO-managed teams
	// when their group claims no longer include that team's sso_team_id.
	// Only affects teams with sso_team_id set; never removes from manually-managed teams.
	// Set via OIDC_REMOVE_FROM_NON_SSO_TEAMS environment variable.
	RemoveFromNonSSOTeams bool
}

// ConfigFromEnv loads TeamSync configuration from environment variables.
func ConfigFromEnv() Config {
	return Config{
		Enabled:               strings.EqualFold(os.Getenv("ENABLE_OIDC_TEAM_SYNC"), "true"),
		RemoveFromNonSSOTeams: strings.EqualFold(os.Getenv("OIDC_REMOVE_FROM_NON_SSO_TEAMS"), "true"),
	}
}

// Service handles automatic team membership synchronization based on SSO group claims.
type Service struct {
	config   Config
	teamRepo *repository.TeamRepository
	orgRepo  *repository.OrganizationRepository
}

// NewService creates a new TeamSyncService.
func NewService(config Config, teamRepo *repository.TeamRepository, orgRepo *repository.OrganizationRepository) *Service {
	return &Service{
		config:   config,
		teamRepo: teamRepo,
		orgRepo:  orgRepo,
	}
}

// IsEnabled returns whether the team sync service is enabled.
func (s *Service) IsEnabled() bool {
	return s.config.Enabled
}

// SyncUserTeams synchronizes a user's team membership based on their SSO group claims.
// This is called on each login when sso_groups are present in the JWT.
//
// The sync logic:
//  1. Find all teams (across all organizations) where sso_team_id matches any group in ssoGroups
//  2. For each matching team: add user as member if not already (also ensure org membership)
//  3. If RemoveFromNonSSOTeams is enabled: remove user from SSO-managed teams whose
//     sso_team_id is no longer in the current ssoGroups
func (s *Service) SyncUserTeams(ctx context.Context, userID uuid.UUID, ssoGroups []string) error {
	if !s.config.Enabled || len(ssoGroups) == 0 {
		return nil
	}

	logger.Infof("TeamSync - Syncing teams for user %s with %d SSO groups: %v", userID, len(ssoGroups), ssoGroups)

	// Find all teams matching the SSO group IDs
	matchingTeams, err := s.teamRepo.FindAllBySSOTeamIDs(ssoGroups)
	if err != nil {
		return fmt.Errorf("failed to find teams by SSO team IDs: %w", err)
	}

	logger.Infof("TeamSync - Found %d matching teams for user %s", len(matchingTeams), userID)

	// Add user to matching teams
	for _, team := range matchingTeams {
		// Ensure org membership first
		if err := s.ensureOrgMembership(userID, team.OrganizationID); err != nil {
			logger.Errorf("TeamSync - Failed to ensure org membership for user %s in org %s: %v", userID, team.OrganizationID, err)
			continue // Don't fail the entire sync for one team
		}

		// Check if user is already a member
		isMember, err := s.teamRepo.IsMember(team.ID, userID)
		if err != nil {
			logger.Errorf("TeamSync - Failed to check membership for user %s in team %s: %v", userID, team.ID, err)
			continue
		}

		if !isMember {
			if err := s.teamRepo.AddMember(team.ID, userID); err != nil {
				logger.Errorf("TeamSync - Failed to add user %s to team %s (%s): %v", userID, team.Name, team.ID, err)
				continue
			}
			logger.Infof("TeamSync - Added user %s to team '%s' (sso_team_id=%s)", userID, team.Name, *team.SSOTeamID)
		}
	}

	// Optionally remove user from SSO-managed teams not in current claims
	if s.config.RemoveFromNonSSOTeams {
		if err := s.removeFromStaleTeams(userID, ssoGroups); err != nil {
			logger.Errorf("TeamSync - Failed to remove user from stale SSO teams: %v", err)
			// Don't return error - additions succeeded
		}
	}

	return nil
}

// ensureOrgMembership ensures the user is a member of the given organization.
// If the user is not already a member, creates an organization membership record.
func (s *Service) ensureOrgMembership(userID, orgID uuid.UUID) error {
	_, err := s.orgRepo.GetMember(orgID, userID)
	if err == nil {
		// Already a member
		return nil
	}
	if err != gorm.ErrRecordNotFound {
		return fmt.Errorf("failed to check org membership: %w", err)
	}

	// Not a member, add them
	if err := s.orgRepo.AddMember(orgID, userID); err != nil {
		// Ignore duplicate key errors (race condition)
		if strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "UNIQUE") {
			return nil
		}
		return fmt.Errorf("failed to add org member: %w", err)
	}

	logger.Debugf("TeamSync - Added user %s as org member of %s", userID, orgID)
	return nil
}

// removeFromStaleTeams removes a user from SSO-managed teams whose sso_team_id
// is no longer in the user's current SSO group claims.
// Only affects teams with sso_team_id set; never touches manually-managed teams.
func (s *Service) removeFromStaleTeams(userID uuid.UUID, currentGroups []string) error {
	// Get all SSO-managed teams the user is currently in
	ssoTeams, err := s.teamRepo.GetSSOTeamsByUserID(userID)
	if err != nil {
		return fmt.Errorf("failed to get user's SSO teams: %w", err)
	}

	// Build a set of current groups for fast lookup
	groupSet := make(map[string]struct{}, len(currentGroups))
	for _, g := range currentGroups {
		groupSet[g] = struct{}{}
	}

	// Remove from teams whose sso_team_id is not in current groups
	for _, team := range ssoTeams {
		if team.SSOTeamID == nil || *team.SSOTeamID == "" {
			continue // Skip non-SSO teams (should not happen due to query, but be safe)
		}

		if _, inGroups := groupSet[*team.SSOTeamID]; !inGroups {
			if err := s.teamRepo.RemoveMember(team.ID, userID); err != nil {
				logger.Errorf("TeamSync - Failed to remove user %s from team '%s' (sso_team_id=%s): %v",
					userID, team.Name, *team.SSOTeamID, err)
				continue
			}
			logger.Debugf("TeamSync - Removed user %s from team '%s' (sso_team_id=%s no longer in claims)",
				userID, team.Name, *team.SSOTeamID)
		}
	}

	return nil
}
