// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package variable

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/uuid"
)

// Note: Service methods in this file can access s.varRepo, s.variableSetRepo, etc.
// from the main Service struct in service.go via method receivers.

// GetPlatformVariablesForAnsibleJob generates platform-provided variables for an Ansible job.
// These are system-generated variables that provide context about the job, project, and organization.
// Platform variables have the lowest priority and can be overridden by user-defined variables.
func (s *Service) GetPlatformVariablesForAnsibleJob(ctx context.Context, projectID uuid.UUID, templateID *uuid.UUID, inventoryID *uuid.UUID, playbookID *uuid.UUID) (map[string]interface{}, error) {
	result := make(map[string]interface{})

	// Generate platform variables with stackweaver_ prefix for Ansible
	// These will be passed as extra_vars to Ansible
	if templateID != nil && *templateID != uuid.Nil {
		result["stackweaver_job_template_id"] = templateID.String()
	}
	if inventoryID != nil && *inventoryID != uuid.Nil {
		result["stackweaver_inventory_id"] = inventoryID.String()
	}
	if playbookID != nil && *playbookID != uuid.Nil {
		result["stackweaver_playbook_id"] = playbookID.String()
	}
	result["stackweaver_project_id"] = projectID.String()

	// Additional platform variables can be added here as needed
	// e.g., project name, organization name, etc. (would require fetching project)

	return result, nil
}

// GetVariablesForAnsibleJob retrieves variables for an Ansible job.
// Precedence order (lowest to highest):
// 1. Platform variables (system-provided, lowest priority)
// 2. Project-assigned variable sets (non-priority, then priority)
// 3. Template-assigned variable sets (non-priority, then priority)
// 4. Template ExtraVars
// 5. Job Override ExtraVars (highest priority)
func (s *Service) GetVariablesForAnsibleJob(ctx context.Context, projectID uuid.UUID, templateID *uuid.UUID, templateExtraVars map[string]interface{}, jobExtraVars map[string]interface{}, inventoryID *uuid.UUID, playbookID *uuid.UUID) (map[string]interface{}, error) {
	result := make(map[string]interface{})
	priorityVars := make(map[string]interface{}) // Priority variables override everything

	// Step 1: Inject platform variables first (lowest priority)
	platformVars, err := s.GetPlatformVariablesForAnsibleJob(ctx, projectID, templateID, inventoryID, playbookID)
	if err != nil {
		// Log error but continue - platform variables are optional
	} else {
		// Platform variables are injected first, so they can be overridden by everything else
		for key, value := range platformVars {
			result[key] = value
		}
	}

	// Step 2: Get variables from variable sets (if variable set repo is available)
	if s.variableSetRepo != nil {
		// Get project-assigned variable sets
		projectVariableSets, err := s.variableSetRepo.ListByProject(projectID)
		if err == nil {
			// Sort variable sets: priority first, then by name (lexical order)
			sort.Slice(projectVariableSets, func(i, j int) bool {
				vsI, vsJ := projectVariableSets[i], projectVariableSets[j]
				// Priority sets come first
				if vsI.Priority != vsJ.Priority {
					return vsI.Priority
				}
				// Then by name (lexical order)
				return vsI.Name < vsJ.Name
			})

			// Process project variable sets - only include env category variables (reused for Ansible)
			for _, vs := range projectVariableSets {
				for _, vsVar := range vs.Variables {
					// Only include env category variables (reused for Ansible)
					if vsVar.Category == "env" {
						value, err := s.GetDecryptedVariableSetValue(&vsVar)
						if err != nil {
							return nil, fmt.Errorf("failed to decrypt variable set variable %s: %w", vsVar.Key, err)
						}

						if vs.Priority {
							// Priority variables override everything
							priorityVars[vsVar.Key] = value
							result[vsVar.Key] = value
						} else {
							// Non-priority: only set if not already set
							if _, exists := result[vsVar.Key]; !exists {
								result[vsVar.Key] = value
							}
						}
					}
				}
			}
		}

		// Note: Template-assigned variable sets removed - variable sets are inherited from project assignments
		// This matches TFE's model where variable sets assigned to projects automatically apply to all templates
		// Organization-scoped variable sets with no project assignments also apply to all projects (included via ListByProject)
	}

	// Step 2.5: Get template variables (if template is specified and template variable repo is available)
	// Template variables override non-priority variable sets but NOT priority variable sets
	// This matches TFE's model: template variables override variable sets (except priority sets)
	if templateID != nil && *templateID != uuid.Nil && s.templateVariableRepo != nil {
		templateVars, err := s.templateVariableRepo.ListByJobTemplate(*templateID)
		if err == nil {
			for _, tv := range templateVars {
				// Only include env category variables for Ansible
				if tv.Category == "env" {
					value, err := s.GetDecryptedTemplateVariableValue(&tv)
					if err != nil {
						return nil, fmt.Errorf("failed to decrypt template variable %s: %w", tv.Key, err)
					}
					// Template variables override non-priority variable sets
					// But priority variable sets override template variables
					if _, isPriority := priorityVars[tv.Key]; !isPriority {
						result[tv.Key] = value
					}
				}
			}
		}
		// Note: If error occurs fetching template variables, we continue with remaining steps
	}

	// Step 3: Merge template ExtraVars (if template is specified)
	// Note: Ranging over nil map is safe in Go (does nothing)
	for key, value := range templateExtraVars {
		// Template extra_vars override non-priority variable sets
		// But priority variable sets override template extra_vars
		if _, isPriority := priorityVars[key]; !isPriority {
			result[key] = value
		}
	}

	// Step 4: Merge job override ExtraVars (highest priority, can override everything except priority sets)
	// Note: Ranging over nil map is safe in Go (does nothing)
	for key, value := range jobExtraVars {
		// Job extra_vars override everything except priority variable sets
		if _, isPriority := priorityVars[key]; !isPriority {
			result[key] = value
		}
	}

	return result, nil
}
