// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package variable

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"sort"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
)

type Service struct {
	varRepo              *repository.VariableRepository
	variableSetRepo      *repository.VariableSetRepository
	workspaceRepo        *repository.WorkspaceRepository
	templateVariableRepo *repository.AnsibleJobTemplateVariableRepository
	encryptionKey        []byte
}

func NewService(varRepo *repository.VariableRepository, encryptionKey []byte) *Service {
	return &Service{
		varRepo:       varRepo,
		encryptionKey: encryptionKey,
	}
}

// NewServiceWithVariableSets creates a service with variable set support
func NewServiceWithVariableSets(varRepo *repository.VariableRepository, variableSetRepo *repository.VariableSetRepository, encryptionKey []byte) *Service {
	return &Service{
		varRepo:         varRepo,
		variableSetRepo: variableSetRepo,
		encryptionKey:   encryptionKey,
	}
}

// NewServiceWithVariableSetsAndWorkspace creates a service with variable set and workspace support (for platform variables)
func NewServiceWithVariableSetsAndWorkspace(varRepo *repository.VariableRepository, variableSetRepo *repository.VariableSetRepository, workspaceRepo *repository.WorkspaceRepository, encryptionKey []byte) *Service {
	return &Service{
		varRepo:         varRepo,
		variableSetRepo: variableSetRepo,
		workspaceRepo:   workspaceRepo,
		encryptionKey:   encryptionKey,
	}
}

// NewServiceWithTemplateVariables creates a service with template variable support (for Ansible)
func NewServiceWithTemplateVariables(varRepo *repository.VariableRepository, variableSetRepo *repository.VariableSetRepository, workspaceRepo *repository.WorkspaceRepository, templateVariableRepo *repository.AnsibleJobTemplateVariableRepository, encryptionKey []byte) *Service {
	return &Service{
		varRepo:              varRepo,
		variableSetRepo:      variableSetRepo,
		workspaceRepo:        workspaceRepo,
		templateVariableRepo: templateVariableRepo,
		encryptionKey:        encryptionKey,
	}
}

func (s *Service) Encrypt(value string) (string, error) {
	if len(s.encryptionKey) != 32 {
		return "", fmt.Errorf("encryption key must be 32 bytes (AES-256)")
	}

	block, err := aes.NewCipher(s.encryptionKey)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(value), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func (s *Service) Decrypt(encryptedValue string) (string, error) {
	if len(s.encryptionKey) != 32 {
		return "", fmt.Errorf("encryption key must be 32 bytes (AES-256)")
	}

	data, err := base64.StdEncoding.DecodeString(encryptedValue)
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(s.encryptionKey)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}

	return string(plaintext), nil
}

func (s *Service) CreateVariable(ctx context.Context, workspaceID string, key, value string, sensitive bool) (*models.Variable, error) {
	var finalValue string
	var encrypted bool

	if sensitive {
		encryptedValue, err := s.Encrypt(value)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt variable: %w", err)
		}
		finalValue = encryptedValue
		encrypted = true
	} else {
		finalValue = value
		encrypted = false
	}

	variable := &models.Variable{
		WorkspaceID: workspaceID,
		Key:         key,
		Value:       finalValue,
		Encrypted:   encrypted,
		Sensitive:   sensitive,
	}

	if err := s.varRepo.Create(variable); err != nil {
		return nil, fmt.Errorf("failed to create variable: %w", err)
	}

	return variable, nil
}

func (s *Service) GetVariable(ctx context.Context, workspaceID string, key string) (*models.Variable, error) {
	return s.varRepo.GetByWorkspaceAndKey(workspaceID, key)
}

func (s *Service) ListVariables(ctx context.Context, workspaceID string) ([]models.Variable, error) {
	return s.varRepo.ListByWorkspace(workspaceID)
}

func (s *Service) GetDecryptedValue(variable *models.Variable) (string, error) {
	if !variable.Encrypted {
		return variable.Value, nil
	}

	return s.Decrypt(variable.Value)
}

func (s *Service) GetDecryptedVariableSetValue(variable *models.VariableSetVariable) (string, error) {
	if !variable.Encrypted {
		return variable.Value, nil
	}

	return s.Decrypt(variable.Value)
}

func (s *Service) GetDecryptedTemplateVariableValue(variable *models.AnsibleJobTemplateVariable) (string, error) {
	if !variable.Encrypted {
		return variable.Value, nil
	}

	return s.Decrypt(variable.Value)
}

// GetPlatformVariablesForWorkspace generates platform-provided variables for a workspace.
// These are system-generated variables that provide context about the workspace, project, and organization.
// Platform variables have the lowest priority and can be overridden by user-defined variables.
func (s *Service) GetPlatformVariablesForWorkspace(ctx context.Context, workspaceID string) (map[string]string, error) {
	result := make(map[string]string)

	// If workspace repo is not available, return empty map (platform variables disabled)
	if s.workspaceRepo == nil {
		return result, nil
	}

	// Fetch workspace with project and organization preloaded
	workspace, err := s.workspaceRepo.GetByID(workspaceID)
	if err != nil {
		return nil, fmt.Errorf("failed to get workspace: %w", err)
	}

	// Generate platform variables
	// TF_ prefix for Terraform input variables (category: "terraform")
	result["TF_WORKSPACE_NAME"] = workspace.Name
	result["TF_WORKSPACE_ID"] = workspace.ID

	// Check if Project is loaded (not zero value) by checking if ID is not nil
	if workspace.Project.ID != uuid.Nil {
		result["TF_PROJECT_NAME"] = workspace.Project.Name
		result["TF_PROJECT_ID"] = workspace.Project.ID.String()

		// Check if Organization is loaded (not zero value) by checking if ID is not nil
		if workspace.Project.Organization.ID != uuid.Nil {
			result["TF_ORGANIZATION_NAME"] = workspace.Project.Organization.Name
			result["TF_ORGANIZATION_ID"] = workspace.Project.Organization.ID.String()
		}
	}

	// Additional platform variables can be added here as needed
	// e.g., execution mode, VCS branch, etc.

	return result, nil
}

// GetPlatformVariableKeys returns a list of platform variable keys for a workspace.
// Used by frontend to show warnings when users create variables that would override platform variables.
func (s *Service) GetPlatformVariableKeys(ctx context.Context, workspaceID string) ([]string, error) {
	platformVars, err := s.GetPlatformVariablesForWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, err
	}

	keys := make([]string, 0, len(platformVars))
	for key := range platformVars {
		keys = append(keys, key)
	}

	// Sort keys for consistent output
	sort.Strings(keys)
	return keys, nil
}

// GetVariablesForRun retrieves Terraform variables (category == "terraform") for a workspace run.
// TFE-compatible precedence order (lowest to highest):
// 1. Non-priority variable sets (organization-scoped, then workspace-scoped, sorted by name)
// 2. Priority variable sets (organization-scoped, then workspace-scoped, sorted by name)
// 3. Workspace variables (can override non-priority sets, but NOT priority sets)
//
// Platform variables (TF_WORKSPACE_ID, TF_WORKSPACE_NAME, etc.) are NOT included here.
// They are injected as environment variables only via GetEnvironmentVariablesForRun,
// matching TFC behavior: env vars avoid "value for undeclared variable" when the root
// module does not declare them. See GetEnvironmentVariablesForRun and GetPlatformVariablesForWorkspace.
func (s *Service) GetVariablesForRun(ctx context.Context, workspaceID string) (map[string]string, error) {
	result := make(map[string]string)
	priorityVars := make(map[string]string) // Priority variables override everything

	// Get variables from variable sets (if variable set repo is available)
	if s.variableSetRepo != nil {
		variableSets, err := s.variableSetRepo.ListByWorkspace(workspaceID)
		if err == nil {
			// Sort variable sets: priority first, then by scope, then by name (lexical order)
			sort.Slice(variableSets, func(i, j int) bool {
				vsI, vsJ := variableSets[i], variableSets[j]
				// Priority sets come first
				if vsI.Priority != vsJ.Priority {
					return vsI.Priority
				}
				// Then by scope: organization before workspace
				if vsI.Scope != vsJ.Scope {
					return vsI.Scope == "organization"
				}
				// Then by name (lexical order)
				return vsI.Name < vsJ.Name
			})

			// Process variable sets - only include terraform category variables
			for _, vs := range variableSets {
				for _, vsVar := range vs.Variables {
					// Only include terraform category variables
					if vsVar.Category == "terraform" {
						value, err := s.GetDecryptedVariableSetValue(&vsVar)
						if err != nil {
							return nil, fmt.Errorf("failed to decrypt variable set variable %s: %w", vsVar.Key, err)
						}

						if vs.Priority {
							// Priority variables override everything (including workspace vars)
							priorityVars[vsVar.Key] = value
							result[vsVar.Key] = value
						} else {
							// Non-priority: only set if not already set (workspace vars will override later)
							if _, exists := result[vsVar.Key]; !exists {
								result[vsVar.Key] = value
							}
						}
					}
				}
			}
		}
		// Note: If error occurs fetching variable sets, we continue with workspace variables only
	}

	// Get workspace variables
	variables, err := s.varRepo.ListByWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}

	for _, v := range variables {
		// Only include terraform category variables (env category variables are handled separately)
		if v.Category == "terraform" {
			value, err := s.GetDecryptedValue(&v)
			if err != nil {
				return nil, fmt.Errorf("failed to decrypt variable %s: %w", v.Key, err)
			}
			// Workspace variables override non-priority variable sets
			// But priority variable sets override workspace variables
			if _, isPriority := priorityVars[v.Key]; !isPriority {
				result[v.Key] = value
			}
		}
	}

	return result, nil
}

// GetEnvironmentVariablesForRun retrieves environment variables (category == "env") for a workspace run.
// TFE-compatible: Environment variables are set as actual environment variables, not in terraform.tfvars.
// Uses same precedence as GetVariablesForRun: platform vars → non-priority sets → priority sets → workspace vars.
func (s *Service) GetEnvironmentVariablesForRun(ctx context.Context, workspaceID string) (map[string]string, error) {
	result := make(map[string]string)
	priorityVars := make(map[string]string) // Priority variables override everything

	// Step 1: Inject platform variables as environment variables (lowest priority).
	// TFC-compatible: TFC sets TFC_RUN_ID, TFC_WORKSPACE_ID, TFC_WORKSPACE_NAME, etc. as env vars only,
	// not in tfvars, so there is no "value for undeclared variable" when the root module does not
	// declare them. We use TF_* keys (TF_WORKSPACE_ID, TF_WORKSPACE_NAME, TF_PROJECT_*, TF_ORGANIZATION_*).
	// To use in Terraform: read from env via external data source, or in provisioners the shell can use $TF_WORKSPACE_ID. As plain
	// env vars they are always available to providers and provisioners.
	platformVars, err := s.GetPlatformVariablesForWorkspace(ctx, workspaceID)
	if err != nil {
		// Log error but continue - platform variables are optional
	} else {
		for key, value := range platformVars {
			result[key] = value
		}
	}

	// Step 2: Get environment variables from variable sets (if variable set repo is available)
	if s.variableSetRepo != nil {
		variableSets, err := s.variableSetRepo.ListByWorkspace(workspaceID)
		if err == nil {
			// Sort variable sets: priority first, then by scope, then by name (lexical order)
			sort.Slice(variableSets, func(i, j int) bool {
				vsI, vsJ := variableSets[i], variableSets[j]
				// Priority sets come first
				if vsI.Priority != vsJ.Priority {
					return vsI.Priority
				}
				// Then by scope: organization before workspace
				if vsI.Scope != vsJ.Scope {
					return vsI.Scope == "organization"
				}
				// Then by name (lexical order)
				return vsI.Name < vsJ.Name
			})

			// Process variable sets - only include env category variables
			for _, vs := range variableSets {
				for _, vsVar := range vs.Variables {
					// Only include env category variables
					if vsVar.Category == "env" {
						value, err := s.GetDecryptedVariableSetValue(&vsVar)
						if err != nil {
							return nil, fmt.Errorf("failed to decrypt variable set variable %s: %w", vsVar.Key, err)
						}

						if vs.Priority {
							// Priority variables override everything (including workspace vars)
							priorityVars[vsVar.Key] = value
							result[vsVar.Key] = value
						} else {
							// Non-priority: only set if not already set (workspace vars will override later)
							if _, exists := result[vsVar.Key]; !exists {
								result[vsVar.Key] = value
							}
						}
					}
				}
			}
		}
		// Note: If error occurs fetching variable sets, we continue with workspace variables only
	}

	// Get workspace environment variables
	variables, err := s.varRepo.ListByWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}

	for _, v := range variables {
		// Only include env category variables
		if v.Category == "env" {
			value, err := s.GetDecryptedValue(&v)
			if err != nil {
				return nil, fmt.Errorf("failed to decrypt variable %s: %w", v.Key, err)
			}
			// Workspace variables override non-priority variable sets
			// But priority variable sets override workspace variables
			if _, isPriority := priorityVars[v.Key]; !isPriority {
				result[v.Key] = value
			}
		}
	}

	return result, nil
}
