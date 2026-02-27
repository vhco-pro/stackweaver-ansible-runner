// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package apikey

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Scope represents a parsed API key scope
type Scope struct {
	Type       string     // "org", "project", "team", "user", or "*" for all
	ResourceID *uuid.UUID // Organization, Project, or Team ID (nil for user scopes or wildcard)
	Permission string     // "read", "write", "admin", etc.
}

// ParseScope parses a scope string into a Scope struct
// Formats:
//   - "*" - all permissions
//   - "org:<org_id>:<permission>" - organization-scoped (e.g., "org:123:read")
//   - "project:<project_id>:<permission>" - project-scoped (e.g., "project:456:write")
//   - "team:<team_id>:<permission>" - team-scoped (e.g., "team:789:read")
//   - "runner:<runner_id>:<permission>" - runner-scoped (e.g., "runner:abc:heartbeat")
//   - "user:<permission>" - user-scoped (e.g., "user:read")
//   - "<permission>" - legacy format, treated as user scope (e.g., "read")
//
// Runner-specific permissions:
//   - "org:<org_id>:runner:register" - allows registering a runner for the organization
//   - "org:<org_id>:runner:terraform" - runner can execute Terraform jobs
//   - "org:<org_id>:runner:ansible" - runner can execute Ansible jobs
//   - "org:<org_id>:runner:combined" - runner can execute both Terraform and Ansible jobs
//   - "runner:<runner_id>:heartbeat" - runner can send heartbeats
//   - "runner:<runner_id>:jobs" - runner can receive and report job status
func ParseScope(scopeStr string) (*Scope, error) {
	scopeStr = strings.TrimSpace(scopeStr)

	// Wildcard scope
	if scopeStr == "*" {
		return &Scope{
			Type:       "*",
			ResourceID: nil,
			Permission: "*",
		}, nil
	}

	parts := strings.Split(scopeStr, ":")

	// Legacy format: just permission (e.g., "read", "write")
	if len(parts) == 1 {
		return &Scope{
			Type:       "user",
			ResourceID: nil,
			Permission: parts[0],
		}, nil
	}

	// Format: "type:resource_id:permission" or "type:permission"
	if len(parts) == 2 {
		// Format: "user:read" or "org:read" (without resource ID - invalid for org/project)
		if parts[0] == "user" {
			return &Scope{
				Type:       "user",
				ResourceID: nil,
				Permission: parts[1],
			}, nil
		}
		// For org/project/team, resource ID is required
		return nil, fmt.Errorf("invalid scope format: %s (resource ID required for %s scopes)", scopeStr, parts[0])
	}

	if len(parts) >= 3 {
		scopeType := parts[0]
		resourceIDStr := parts[1]
		// Join remaining parts as the permission to support compound permissions
		// e.g., "org:<org_id>:runner:register" -> permission = "runner:register"
		permission := strings.Join(parts[2:], ":")

		if scopeType != "org" && scopeType != "project" && scopeType != "team" && scopeType != "runner" {
			return nil, fmt.Errorf("invalid scope type: %s (must be 'org', 'project', 'team', 'runner', or 'user')", scopeType)
		}

		resourceID, err := uuid.Parse(resourceIDStr)
		if err != nil {
			return nil, fmt.Errorf("invalid resource ID in scope: %s", resourceIDStr)
		}

		return &Scope{
			Type:       scopeType,
			ResourceID: &resourceID,
			Permission: permission,
		}, nil
	}

	return nil, fmt.Errorf("invalid scope format: %s", scopeStr)
}

// ParseScopes parses multiple scope strings
func ParseScopes(scopeStrs []string) ([]*Scope, error) {
	scopes := make([]*Scope, 0, len(scopeStrs))
	for _, scopeStr := range scopeStrs {
		scope, err := ParseScope(scopeStr)
		if err != nil {
			return nil, err
		}
		scopes = append(scopes, scope)
	}
	return scopes, nil
}

// HasPermission checks if a scope grants a specific permission
func (s *Scope) HasPermission(permission string) bool {
	if s.Permission == "*" {
		return true
	}
	return s.Permission == permission
}

// MatchesResource checks if a scope matches a specific resource
func (s *Scope) MatchesResource(resourceType string, resourceID *uuid.UUID) bool {
	// Wildcard matches everything
	if s.Type == "*" {
		return true
	}

	// Type must match
	if s.Type != resourceType {
		return false
	}

	// For user scopes, resourceID is nil and matches any user context
	if s.Type == "user" {
		return true
	}

	// For org/project scopes, resource ID must match
	if s.ResourceID == nil {
		return false
	}
	if resourceID == nil {
		return false
	}
	return s.ResourceID.String() == resourceID.String()
}

// ScopeChecker provides methods to check permissions for API keys
type ScopeChecker struct {
	scopes []*Scope
}

// NewScopeChecker creates a new scope checker from scope strings
func NewScopeChecker(scopeStrs []string) (*ScopeChecker, error) {
	scopes, err := ParseScopes(scopeStrs)
	if err != nil {
		return nil, err
	}
	return &ScopeChecker{scopes: scopes}, nil
}

// HasPermission checks if any scope grants the requested permission
// resourceType: "org", "project", or "user"
// resourceID: Organization or Project ID (nil for user scopes)
// permission: "read", "write", "admin", etc.
func (sc *ScopeChecker) HasPermission(resourceType string, resourceID *uuid.UUID, permission string) bool {
	// Empty scopes means full access (backward compatibility)
	if len(sc.scopes) == 0 {
		return true
	}

	for _, scope := range sc.scopes {
		// Check if scope matches resource
		if !scope.MatchesResource(resourceType, resourceID) {
			continue
		}

		// Check if scope grants permission
		if scope.HasPermission(permission) {
			return true
		}
	}

	return false
}

// HasOrgPermission checks if the scopes grant permission for an organization
func (sc *ScopeChecker) HasOrgPermission(orgID uuid.UUID, permission string) bool {
	return sc.HasPermission("org", &orgID, permission)
}

// HasProjectPermission checks if the scopes grant permission for a project
func (sc *ScopeChecker) HasProjectPermission(projectID uuid.UUID, permission string) bool {
	return sc.HasPermission("project", &projectID, permission)
}

// HasUserPermission checks if the scopes grant permission for user operations
func (sc *ScopeChecker) HasUserPermission(permission string) bool {
	return sc.HasPermission("user", nil, permission)
}

// GetScopedOrganizations returns all organization IDs that are explicitly scoped
func (sc *ScopeChecker) GetScopedOrganizations() []uuid.UUID {
	orgIDs := make([]uuid.UUID, 0)
	for _, scope := range sc.scopes {
		if scope.Type == "org" && scope.ResourceID != nil {
			orgIDs = append(orgIDs, *scope.ResourceID)
		}
	}
	return orgIDs
}

// GetScopedProjects returns all project IDs that are explicitly scoped
func (sc *ScopeChecker) GetScopedProjects() []uuid.UUID {
	projectIDs := make([]uuid.UUID, 0)
	for _, scope := range sc.scopes {
		if scope.Type == "project" && scope.ResourceID != nil {
			projectIDs = append(projectIDs, *scope.ResourceID)
		}
	}
	return projectIDs
}

// GetScopedTeams returns all team IDs that are explicitly scoped
func (sc *ScopeChecker) GetScopedTeams() []uuid.UUID {
	teamIDs := make([]uuid.UUID, 0)
	for _, scope := range sc.scopes {
		if scope.Type == "team" && scope.ResourceID != nil {
			teamIDs = append(teamIDs, *scope.ResourceID)
		}
	}
	return teamIDs
}

// GetScopedRunners returns all runner IDs that are explicitly scoped
func (sc *ScopeChecker) GetScopedRunners() []uuid.UUID {
	runnerIDs := make([]uuid.UUID, 0)
	for _, scope := range sc.scopes {
		if scope.Type == "runner" && scope.ResourceID != nil {
			runnerIDs = append(runnerIDs, *scope.ResourceID)
		}
	}
	return runnerIDs
}

// HasRunnerRegisterPermission checks if the scopes grant runner:register for an organization
func (sc *ScopeChecker) HasRunnerRegisterPermission(orgID uuid.UUID) bool {
	return sc.HasOrgPermission(orgID, "runner:register")
}

// HasTeamPermission checks if the scopes grant permission for a team
func (sc *ScopeChecker) HasTeamPermission(teamID uuid.UUID, permission string) bool {
	return sc.HasPermission("team", &teamID, permission)
}

// HasRunnerPermission checks if the scopes grant permission for a runner
func (sc *ScopeChecker) HasRunnerPermission(runnerID uuid.UUID, permission string) bool {
	return sc.HasPermission("runner", &runnerID, permission)
}

// IsUnrestricted returns true if the scopes allow unrestricted access (empty or wildcard)
func (sc *ScopeChecker) IsUnrestricted() bool {
	if len(sc.scopes) == 0 {
		return true
	}
	for _, scope := range sc.scopes {
		if scope.Type == "*" {
			return true
		}
	}
	return false
}
