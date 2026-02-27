// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package state

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/storage"
	"github.com/michielvha/logger"
)

type Service struct {
	stateVersionRepo *repository.StateVersionRepository
	stateLockRepo    *repository.StateLockRepository
	workspaceRepo    *repository.WorkspaceRepository
	storageClient    storage.Client
}

func NewService(
	stateVersionRepo *repository.StateVersionRepository,
	stateLockRepo *repository.StateLockRepository,
	workspaceRepo *repository.WorkspaceRepository,
	storageClient storage.Client,
) *Service {
	return &Service{
		stateVersionRepo: stateVersionRepo,
		stateLockRepo:    stateLockRepo,
		workspaceRepo:    workspaceRepo,
		storageClient:    storageClient,
	}
}

func (s *Service) GetLatestState(ctx context.Context, workspaceID string) (*models.StateVersion, error) {
	return s.stateVersionRepo.GetLatest(workspaceID)
}

// CheckWorkspaceLock checks if a workspace is manually locked
func (s *Service) CheckWorkspaceLock(ctx context.Context, workspaceID string) error {
	workspace, err := s.workspaceRepo.GetByID(workspaceID)
	if err != nil {
		return fmt.Errorf("failed to get workspace: %w", err)
	}
	if workspace.Locked {
		if workspace.LockedReason != "" {
			return fmt.Errorf("workspace is manually locked: %s", workspace.LockedReason)
		}
		return fmt.Errorf("workspace is manually locked")
	}
	return nil
}

func (s *Service) SaveState(ctx context.Context, workspaceID string, stateData map[string]interface{}, runID *string, commitHash string, committer string) (*models.StateVersion, error) {
	// 1. Check if workspace is manually locked
	if err := s.CheckWorkspaceLock(ctx, workspaceID); err != nil {
		return nil, fmt.Errorf("workspace lock check failed: %w", err)
	}

	// 2. Check state lock
	existingLock, err := s.stateLockRepo.GetByWorkspace(workspaceID)
	if err == nil && !existingLock.IsExpired() {
		// Lock exists and is not expired
		if runID != nil {
			// If runID is provided, verify the lock belongs to this run
			if existingLock.LockedBy == nil || *existingLock.LockedBy != *runID {
				return nil, fmt.Errorf("state is locked by another operation (lock ID: %s, locked by: %s)", existingLock.LockID, *existingLock.LockedBy)
			}
			// Lock belongs to this run, proceed
		} else {
			// Manual state save without runID - must not have any active locks
			return nil, fmt.Errorf("state is locked by run %v (lock ID: %s)", existingLock.LockedBy, existingLock.LockID)
		}
	}

	// 3. Get next version number
	version, err := s.stateVersionRepo.GetNextVersion(workspaceID)
	if err != nil {
		return nil, fmt.Errorf("failed to get next version: %w", err)
	}

	// Create state version
	stateVersion := &models.StateVersion{
		WorkspaceID: workspaceID,
		RunID:       runID, // Link to the run that created this state version
		Version:     version,
		StateData:   models.StateData(stateData),
		CommitHash:  commitHash, // Git commit hash (empty if not available)
		Committer:   committer,  // Committer email/name (empty if not available)
	}

	if err := s.stateVersionRepo.Create(stateVersion); err != nil {
		return nil, fmt.Errorf("failed to create state version: %w", err)
	}

	// Also save to object storage
	stateJSON, err := json.Marshal(stateData)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal state: %w", err)
	}

	key := fmt.Sprintf("workspaces/%s/state/%d.json", workspaceID, version)
	if err := s.storageClient.Put(ctx, key, stateJSON); err != nil {
		return nil, fmt.Errorf("failed to save state to storage: %w", err)
	}

	return stateVersion, nil
}

func (s *Service) LockState(ctx context.Context, workspaceID string, lockID string, operation string, lockedBy *string, ttl time.Duration) error {
	// Check for existing lock
	existingLock, err := s.stateLockRepo.GetByWorkspaceAndLockID(workspaceID, lockID)
	if err == nil && !existingLock.IsExpired() {
		return fmt.Errorf("state is already locked")
	}

	// Create new lock
	lock := &models.StateLock{
		WorkspaceID: workspaceID,
		LockID:      lockID,
		Operation:   operation,
		LockedBy:    lockedBy,
		ExpiresAt:   time.Now().Add(ttl),
	}

	if err := s.stateLockRepo.Create(lock); err != nil {
		return fmt.Errorf("failed to create lock: %w", err)
	}

	return nil
}

func (s *Service) UnlockState(ctx context.Context, workspaceID string, lockID string) error {
	return s.stateLockRepo.Delete(workspaceID, lockID)
}

// GetStateLock returns the active state lock for a workspace (if any)
func (s *Service) GetStateLock(ctx context.Context, workspaceID string) (*models.StateLock, error) {
	return s.stateLockRepo.GetByWorkspace(workspaceID)
}

func (s *Service) ListVersions(ctx context.Context, workspaceID string, limit, offset int) ([]models.StateVersion, int64, error) {
	return s.stateVersionRepo.ListByWorkspace(workspaceID, limit, offset)
}

func (s *Service) CleanupExpiredLocks(ctx context.Context) error {
	return s.stateLockRepo.CleanupExpiredLocks()
}

// DeleteStateVersion deletes a state version and its associated storage
func (s *Service) DeleteStateVersion(ctx context.Context, stateVersionID string) error {
	// 1. Get state version
	stateVersion, err := s.stateVersionRepo.GetByID(stateVersionID)
	if err != nil {
		return fmt.Errorf("failed to get state version: %w", err)
	}

	// 2. Check if workspace is manually locked
	if err := s.CheckWorkspaceLock(ctx, stateVersion.WorkspaceID); err != nil {
		return fmt.Errorf("workspace lock check failed: %w", err)
	}

	// 3. Check state lock - must not be locked by an active run (if this is the latest state)
	latestState, err := s.stateVersionRepo.GetLatest(stateVersion.WorkspaceID)
	if err == nil && latestState != nil && latestState.ID == stateVersionID {
		// This is the latest state version - check for locks
		existingLock, err := s.stateLockRepo.GetByWorkspace(stateVersion.WorkspaceID)
		if err == nil && existingLock != nil && !existingLock.IsExpired() {
			return fmt.Errorf("state is locked by run %v (lock ID: %s). Cannot delete latest state version while locked", existingLock.LockedBy, existingLock.LockID)
		}
	}

	// 4. Delete from storage (MinIO)
	storageKey := fmt.Sprintf("workspaces/%s/state/%d.json", stateVersion.WorkspaceID, stateVersion.Version)
	if err := s.storageClient.Delete(ctx, storageKey); err != nil {
		// Log error but continue - state file might not exist in storage if it was only in DB
		// This is not critical since we're deleting the version anyway
		logger.Warnf("Failed to delete state file from storage %s: %v", storageKey, err)
	}

	// 5. Delete from database
	if err := s.stateVersionRepo.Delete(stateVersionID); err != nil {
		return fmt.Errorf("failed to delete state version: %w", err)
	}

	return nil
}

// RemoveResourceFromState removes a resource from the latest state version by address
// This is equivalent to "terraform state rm <address>"
func (s *Service) RemoveResourceFromState(ctx context.Context, workspaceID string, resourceAddress string) error {
	// 1. Check if workspace is manually locked
	if err := s.CheckWorkspaceLock(ctx, workspaceID); err != nil {
		return fmt.Errorf("workspace lock check failed: %w", err)
	}

	// 2. Check state lock - must not be locked by an active run
	existingLock, err := s.stateLockRepo.GetByWorkspace(workspaceID)
	if err == nil && existingLock != nil && !existingLock.IsExpired() {
		return fmt.Errorf("state is locked by run %v (lock ID: %s). Cannot modify state while locked", existingLock.LockedBy, existingLock.LockID)
	}

	// 3. Get latest state version
	latestState, err := s.stateVersionRepo.GetLatest(workspaceID)
	if err != nil {
		return fmt.Errorf("failed to get latest state: %w", err)
	}

	// 4. Load state data - try from StateData first, fall back to MinIO
	stateData := map[string]interface{}(latestState.StateData)
	if len(stateData) == 0 {
		// Load from MinIO
		storageKey := fmt.Sprintf("workspaces/%s/state/%d.json", workspaceID, latestState.Version)
		stateBytes, err := s.storageClient.Get(ctx, storageKey)
		if err != nil {
			return fmt.Errorf("failed to load state from storage: %w", err)
		}
		if err := json.Unmarshal(stateBytes, &stateData); err != nil {
			return fmt.Errorf("failed to parse state JSON: %w", err)
		}
	}

	// 5. Remove resource from state
	removed, err := s.removeResourceFromStateData(stateData, resourceAddress)
	if err != nil {
		return fmt.Errorf("failed to remove resource: %w", err)
	}
	if !removed {
		return fmt.Errorf("resource not found in state: %s", resourceAddress)
	}

	// 6. Update serial number (Terraform increments serial on state changes)
	if serial, ok := stateData["serial"].(float64); ok {
		newSerial := int(serial) + 1
		stateData["serial"] = newSerial
	} else if serialInt, ok := stateData["serial"].(int); ok {
		stateData["serial"] = serialInt + 1
	} else {
		// If no serial, set to 1
		stateData["serial"] = 1
	}

	// 7. Save modified state as new version
	_, err = s.SaveState(ctx, workspaceID, stateData, nil, latestState.CommitHash, latestState.Committer)
	if err != nil {
		return fmt.Errorf("failed to save modified state: %w", err)
	}

	return nil
}

// removeResourceFromStateData removes a resource from state data by address
// Returns true if resource was found and removed, false otherwise
func (s *Service) removeResourceFromStateData(stateData map[string]interface{}, resourceAddress string) (bool, error) {
	// Terraform state structure:
	// {
	//   "version": 4,
	//   "terraform_version": "...",
	//   "lineage": "...",
	//   "serial": 123,
	//   "outputs": {...},
	//   "resources": [
	//     {
	//       "mode": "managed",
	//       "type": "...",
	//       "name": "...",
	//       "provider": "...",
	//       "instances": [...]
	//     }
	//   ]
	// }
	// OR with modules:
	// {
	//   "version": 4,
	//   "terraform_version": "...",
	//   "lineage": "...",
	//   "serial": 123,
	//   "outputs": {...},
	//   "resources": [
	//     {
	//       "mode": "managed",
	//       "type": "...",
	//       "name": "...",
	//       "provider": "...",
	//       "module": "module.submodule", // optional
	//       "instances": [...]
	//     }
	//   ]
	// }
	// OR with root_module structure:
	// {
	//   "version": 4,
	//   "terraform_version": "...",
	//   "lineage": "...",
	//   "serial": 123,
	//   "outputs": {...},
	//   "root_module": {
	//     "resources": [...],
	//     "child_modules": [...]
	//   }
	// }

	// Handle both flat resources array and root_module structure
	if rootModule, ok := stateData["root_module"].(map[string]interface{}); ok {
		// New format with root_module
		return s.removeResourceFromModule(rootModule, resourceAddress, "")
	}

	// Old format with flat resources array
	if resources, ok := stateData["resources"].([]interface{}); ok {
		return s.removeResourceFromFlatArray(resources, resourceAddress, stateData)
	}

	return false, fmt.Errorf("unexpected state format")
}

// removeResourceFromModule removes a resource from a module (root_module or child_module)
func (s *Service) removeResourceFromModule(module map[string]interface{}, resourceAddress string, modulePrefix string) (bool, error) {
	// Check resources in this module
	if resources, ok := module["resources"].([]interface{}); ok {
		filteredResources := make([]interface{}, 0)
		removed := false

		for _, resource := range resources {
			resourceMap, ok := resource.(map[string]interface{})
			if !ok {
				filteredResources = append(filteredResources, resource)
				continue
			}

			// Check address field first (most reliable)
			if resourceAddr, ok := resourceMap["address"].(string); ok && resourceAddr == resourceAddress {
				// Found the resource to remove by address
				removed = true
				continue // Skip this resource
			}

			// Fallback: match by type and name (construct address from type.name)
			resourceType, _ := resourceMap["type"].(string)
			resourceName, _ := resourceMap["name"].(string)

			// Construct expected address from parts
			if resourceType != "" && resourceName != "" {
				// Check if address matches (with or without module prefix)
				constructedAddress := fmt.Sprintf("%s.%s", resourceType, resourceName)
				addressWithModule := constructedAddress
				if modulePrefix != "" && modulePrefix != "root" {
					addressWithModule = fmt.Sprintf("%s.%s", modulePrefix, constructedAddress)
				}

				// Match exact address or constructed address (with or without module)
				if resourceAddress == constructedAddress || resourceAddress == addressWithModule {
					removed = true
					continue // Skip this resource
				}
			}

			filteredResources = append(filteredResources, resource)
		}

		if removed {
			module["resources"] = filteredResources
			return true, nil
		}
	}

	// Check child modules
	if childModules, ok := module["child_modules"].([]interface{}); ok {
		for _, childModule := range childModules {
			childModuleMap, ok := childModule.(map[string]interface{})
			if !ok {
				continue
			}

			// Get child module address
			childAddress, _ := childModuleMap["address"].(string)
			if childAddress == "" {
				continue
			}

			removed, err := s.removeResourceFromModule(childModuleMap, resourceAddress, childAddress)
			if err != nil {
				return false, err
			}
			if removed {
				return true, nil
			}
		}
	}

	return false, nil
}

// removeResourceFromFlatArray removes a resource from flat resources array (old format)
func (s *Service) removeResourceFromFlatArray(resources []interface{}, resourceAddress string, stateData map[string]interface{}) (bool, error) {
	addressParts := s.parseResourceAddress(resourceAddress)
	expectedType := addressParts["type"]
	expectedName := addressParts["name"]
	expectedModule := addressParts["module"]

	filteredResources := make([]interface{}, 0)
	removed := false

	for _, resource := range resources {
		resourceMap, ok := resource.(map[string]interface{})
		if !ok {
			filteredResources = append(filteredResources, resource)
			continue
		}

		// Get resource type, name, and module
		resourceType, _ := resourceMap["type"].(string)
		resourceName, _ := resourceMap["name"].(string)
		resourceModule, _ := resourceMap["module"].(string)

		// Check if this is the resource to remove
		matchType := expectedType != "" && resourceType == expectedType
		matchName := expectedName != "" && resourceName == expectedName
		matchModule := expectedModule == "" || resourceModule == expectedModule

		// Also check address field if present
		resourceAddressField, _ := resourceMap["address"].(string)
		matchAddress := resourceAddressField == resourceAddress

		if (matchType && matchName && matchModule) || matchAddress {
			// Found the resource to remove
			removed = true
			continue // Skip this resource
		}

		filteredResources = append(filteredResources, resource)
	}

	if removed {
		stateData["resources"] = filteredResources
		return true, nil
	}

	return false, nil
}

// parseResourceAddress parses a Terraform resource address into parts
// Examples:
//   - "tfe_team_organization_member.test_team_member" -> {type: "tfe_team_organization_member", name: "test_team_member", module: ""}
//   - "module.submodule.tfe_team.test" -> {type: "tfe_team", name: "test", module: "module.submodule"}
func (s *Service) parseResourceAddress(address string) map[string]string {
	parts := map[string]string{
		"type":   "",
		"name":   "",
		"module": "",
	}

	// Split by dots
	segments := strings.Split(address, ".")

	if len(segments) < 2 {
		return parts // Invalid address format
	}

	// Check if starts with "module"
	if segments[0] == "module" {
		// Find where the resource type starts (after all module segments)
		moduleSegments := []string{}
		i := 1
		for i < len(segments)-2 && segments[i] != "" {
			moduleSegments = append(moduleSegments, segments[i])
			i++
		}

		if len(segments) >= i+2 {
			parts["module"] = strings.Join(append([]string{"module"}, moduleSegments...), ".")
			parts["type"] = segments[i]
			parts["name"] = strings.Join(segments[i+1:], ".")
		}
	} else {
		// Simple format: "type.name" or "type.name.index"
		parts["type"] = segments[0]
		parts["name"] = strings.Join(segments[1:], ".")
	}

	return parts
}
