// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package logparser

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/iac-platform/backend/internal/models"
)

// PlannedResource represents a resource from the plan output
type PlannedResource struct {
	Address string
	Action  string // create, update, delete, replace
}

// ParseResult represents the parsed state of a phase
type ParseResult struct {
	Resources models.ResourceStates
	Summary   models.PhaseSummary
}

// ParseApplyLogs parses Terraform apply logs to extract resource states
// This mirrors the frontend parsing logic in ApplyOutputViewer.tsx
// plannedResources can be extracted from plan output's resource_changes array
func ParseApplyLogs(logs string, plannedResources []PlannedResource) (*ParseResult, error) {
	if logs == "" {
		return &ParseResult{
			Resources: make(models.ResourceStates, 0),
			Summary:   models.PhaseSummary{},
		}, nil
	}

	lines := strings.Split(logs, "\n")
	resourceStatuses := make(map[string]string) // address -> status
	resources := make([]models.ResourceState, 0)
	destroyedResources := make(map[string]bool) // Track destroyed resources for replace detection
	var summaryLine *struct {
		added     int
		changed   int
		destroyed int
	}

	// Initialize statuses from planned resources
	for _, planned := range plannedResources {
		if _, exists := resourceStatuses[planned.Address]; !exists {
			resourceStatuses[planned.Address] = "pending"
		}
	}

	// Regex patterns (matching frontend patterns)
	creatingPattern := regexp.MustCompile(`^([\w._-]+):\s+Creating`)
	createCompletePattern := regexp.MustCompile(`^([\w._-]+):\s+Creation complete after .*?(?:\[id=([^\]]+)\])?`)
	modifyingPattern := regexp.MustCompile(`^([\w._-]+):\s+Modifying`)
	modifyCompletePattern := regexp.MustCompile(`^([\w._-]+):\s+Modifications? complete after .*?(?:\[id=([^\]]+)\])?`)
	destroyingPattern := regexp.MustCompile(`^([\w._-]+):\s+Destroying`)
	destroyCompletePattern := regexp.MustCompile(`^([\w._-]+):\s+Destruction complete after`)
	summaryPattern := regexp.MustCompile(`Apply complete! Resources: (\d+) added, (\d+) changed, (\d+) destroyed`)

	// Error patterns - parse errors to mark resources as failed
	// Pattern 1: "Error: <message> on <resource>" - specific resource error
	resourceErrorPattern := regexp.MustCompile(`Error:\s+(.+?)\s+on\s+([\w._-]+)`)
	// Pattern 2: "Error: <message>" followed by "on <file> line <line>, in <resource>:" - multi-line error
	errorOnResourcePattern := regexp.MustCompile(`on\s+[^,]+,\s+in\s+([\w._-]+):`)
	// Pattern 3: JSON log format: "@level":"error" with "@message" containing resource
	jsonErrorPattern := regexp.MustCompile(`"@level"\s*:\s*"error"`)

	// Parse logs line by line
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// Match resource creation starting
		if match := creatingPattern.FindStringSubmatch(trimmed); match != nil {
			address := match[1]
			resourceStatuses[address] = "applying"
			continue
		}

		// Match resource creation complete
		if match := createCompletePattern.FindStringSubmatch(trimmed); match != nil {
			address := match[1]
			id := ""
			if len(match) > 2 && match[2] != "" {
				id = match[2]
			} else {
				// Try to extract ID from resource.id pattern in surrounding lines
				id = extractResourceID(lines, address)
			}

			resourceStatuses[address] = "completed"

			// Check if this resource was destroyed first (replace operation)
			wasDestroyed := destroyedResources[address]
			action := "create"
			if wasDestroyed {
				action = "replace"
				delete(destroyedResources, address)
			}

			// Extract creation time if available
			var createdAt *time.Time
			if createdTime := extractCreationTime(trimmed); createdTime != nil {
				createdAt = createdTime
			}

			resources = append(resources, models.ResourceState{
				Address:    address,
				Status:     "completed",
				ResourceID: id,
				CreatedAt:  createdAt,
				Action:     action,
				Details:    trimmed,
			})
			continue
		}

		// Match resource modification starting
		if match := modifyingPattern.FindStringSubmatch(trimmed); match != nil {
			address := match[1]
			resourceStatuses[address] = "applying"
			continue
		}

		// Match resource modification/update complete
		if match := modifyCompletePattern.FindStringSubmatch(trimmed); match != nil {
			address := match[1]
			id := ""
			if len(match) > 2 && match[2] != "" {
				id = match[2]
			} else {
				id = extractResourceID(lines, address)
			}

			resourceStatuses[address] = "completed"

			// Check if this resource was destroyed first (replace operation)
			wasDestroyed := destroyedResources[address]
			action := "update"
			if wasDestroyed {
				action = "replace"
				delete(destroyedResources, address)
			}

			resources = append(resources, models.ResourceState{
				Address:    address,
				Status:     "completed",
				ResourceID: id,
				Action:     action,
				Details:    trimmed,
			})
			continue
		}

		// Match resource destruction starting
		if match := destroyingPattern.FindStringSubmatch(trimmed); match != nil {
			address := match[1]
			resourceStatuses[address] = "applying"
			continue
		}

		// Match resource destruction complete
		if match := destroyCompletePattern.FindStringSubmatch(trimmed); match != nil {
			address := match[1]
			resourceStatuses[address] = "completed"

			// Track this as a potential replace (will be removed if we see creation)
			destroyedResources[address] = true

			resources = append(resources, models.ResourceState{
				Address: address,
				Status:  "completed",
				Action:  "delete",
				Details: trimmed,
			})
			continue
		}

		// Match errors - parse errors to mark resources as failed
		// Pattern 1: "Error: <message> on <resource>" - specific resource error
		if match := resourceErrorPattern.FindStringSubmatch(trimmed); match != nil {
			errorMessage := match[1]
			partialAddress := match[2]
			// Try to find matching full address from planned resources
			// Error messages often contain partial addresses (without module prefixes)
			resourceAddress := partialAddress
			for _, planned := range plannedResources {
				// Check if planned address ends with the partial address
				// e.g., "module.proxmox_test.proxmox_virtual_environment_download_file.test_iso" ends with "proxmox_virtual_environment_download_file.test_iso"
				if strings.HasSuffix(planned.Address, "."+partialAddress) || planned.Address == partialAddress {
					resourceAddress = planned.Address
					break
				}
			}
			// Mark this resource as failed
			resourceStatuses[resourceAddress] = "failed"
			// Update or add resource with error
			found := false
			for i := range resources {
				if resources[i].Address == resourceAddress {
					resources[i].Status = "failed"
					resources[i].ErrorMessage = errorMessage
					resources[i].Details = trimmed
					found = true
					break
				}
			}
			if !found {
				// Determine action from planned resources
				action := "unknown"
				for _, planned := range plannedResources {
					if planned.Address == resourceAddress {
						action = planned.Action
						break
					}
				}
				resources = append(resources, models.ResourceState{
					Address:      resourceAddress,
					Status:       "failed",
					Action:       action,
					ErrorMessage: errorMessage,
					Details:      trimmed,
				})
			}
			continue
		}

		// Pattern 2: "Error: <message>" - general error, check next lines for resource
		if strings.HasPrefix(trimmed, "Error:") {
			errorMessage := strings.TrimPrefix(trimmed, "Error:")
			errorMessage = strings.TrimSpace(errorMessage)
			// Look ahead in next few lines for resource address
			resourceAddress := ""
			for j := i + 1; j < len(lines) && j < i+5; j++ {
				nextLine := strings.TrimSpace(lines[j])
				// Check for "on <file> line <line>, in <resource>:"
				if match := errorOnResourcePattern.FindStringSubmatch(nextLine); match != nil {
					resourceAddress = match[1]
					break
				}
				// Check for "with <resource>,"
				if strings.Contains(nextLine, "with ") && strings.Contains(nextLine, ",") {
					parts := strings.Split(nextLine, "with ")
					if len(parts) > 1 {
						resourcePart := strings.Split(parts[1], ",")[0]
						resourceAddress = strings.TrimSpace(resourcePart)
						break
					}
				}
			}
			if resourceAddress != "" {
				// Mark this resource as failed
				resourceStatuses[resourceAddress] = "failed"
				// Update or add resource with error
				found := false
				for i := range resources {
					if resources[i].Address == resourceAddress {
						resources[i].Status = "failed"
						resources[i].ErrorMessage = errorMessage
						resources[i].Details = trimmed
						found = true
						break
					}
				}
				if !found {
					// Determine action from planned resources
					action := "unknown"
					for _, planned := range plannedResources {
						if planned.Address == resourceAddress {
							action = planned.Action
							break
						}
					}
					resources = append(resources, models.ResourceState{
						Address:      resourceAddress,
						Status:       "failed",
						Action:       action,
						ErrorMessage: errorMessage,
						Details:      trimmed,
					})
				}
			}
			continue
		}

		// Pattern 3: JSON log format - check for error level logs
		if jsonErrorPattern.MatchString(trimmed) {
			// Try to extract resource from JSON log
			// Look for "@message" containing resource address
			// This is a simplified check - full JSON parsing would be more robust
			for _, planned := range plannedResources {
				if strings.Contains(trimmed, planned.Address) {
					// Mark this resource as failed
					resourceStatuses[planned.Address] = "failed"
					// Extract error message if possible
					errorMessage := trimmed
					if strings.Contains(trimmed, `"@message"`) {
						// Try to extract message value
						msgStart := strings.Index(trimmed, `"@message"`)
						if msgStart >= 0 {
							msgPart := trimmed[msgStart:]
							if colonIdx := strings.Index(msgPart, ":"); colonIdx >= 0 {
								msgValue := strings.TrimSpace(msgPart[colonIdx+1:])
								// Remove quotes if present
								msgValue = strings.Trim(msgValue, `"`)
								if len(msgValue) > 0 && len(msgValue) < 200 {
									errorMessage = msgValue
								}
							}
						}
					}
					// Update or add resource with error
					found := false
					for i := range resources {
						if resources[i].Address == planned.Address {
							resources[i].Status = "failed"
							resources[i].ErrorMessage = errorMessage
							resources[i].Details = trimmed
							found = true
							break
						}
					}
					if !found {
						resources = append(resources, models.ResourceState{
							Address:      planned.Address,
							Status:       "failed",
							Action:       planned.Action,
							ErrorMessage: errorMessage,
							Details:      trimmed,
						})
					}
					break
				}
			}
			continue
		}

		// Match final summary line
		if match := summaryPattern.FindStringSubmatch(trimmed); match != nil {
			added, _ := strconv.Atoi(match[1])
			changed, _ := strconv.Atoi(match[2])
			destroyed, _ := strconv.Atoi(match[3])
			summaryLine = &struct {
				added     int
				changed   int
				destroyed int
			}{
				added:     added,
				changed:   changed,
				destroyed: destroyed,
			}
			continue
		}
	}

	// Build summary
	summary := models.PhaseSummary{}
	if summaryLine != nil {
		summary.Additions = summaryLine.added
		summary.Changes = summaryLine.changed
		summary.Destructions = summaryLine.destroyed
	} else {
		// Calculate from resources if summary line not found
		for _, res := range resources {
			switch res.Action {
			case "create":
				summary.Additions++
			case "update":
				summary.Changes++
			case "delete":
				summary.Destructions++
			case "replace":
				summary.Replace++
			}
		}
	}

	// Count failed resources (resources that were applying but never completed)
	failedCount := 0
	for address, status := range resourceStatuses {
		if status == "applying" {
			// Mark as failed if it was applying but never completed
			failedCount++
			// Add failed resource to list
			found := false
			for i := range resources {
				if resources[i].Address == address {
					resources[i].Status = "failed"
					found = true
					break
				}
			}
			if !found {
				resources = append(resources, models.ResourceState{
					Address: address,
					Status:  "failed",
					Action:  "unknown",
				})
			}
		}
	}
	summary.Failed = failedCount
	summary.Total = len(resources)

	return &ParseResult{
		Resources: resources,
		Summary:   summary,
	}, nil
}

// ExtractPlannedResourcesFromPlanOutput extracts planned resources from plan output JSON
// Plan output has resource_changes array with address and change.actions
func ExtractPlannedResourcesFromPlanOutput(planOutput map[string]interface{}) []PlannedResource {
	plannedResources := make([]PlannedResource, 0)

	if planOutput == nil {
		return plannedResources
	}

	resourceChanges, ok := planOutput["resource_changes"].([]interface{})
	if !ok {
		return plannedResources
	}

	for _, change := range resourceChanges {
		changeMap, ok := change.(map[string]interface{})
		if !ok {
			continue
		}

		address, ok := changeMap["address"].(string)
		if !ok {
			continue
		}

		// Extract action from change.actions array
		action := "unknown"
		if changeData, ok := changeMap["change"].(map[string]interface{}); ok {
			if actions, ok := changeData["actions"].([]interface{}); ok && len(actions) > 0 {
				if actionStr, ok := actions[0].(string); ok {
					action = actionStr
				}
			}
		}

		plannedResources = append(plannedResources, PlannedResource{
			Address: address,
			Action:  action,
		})
	}

	return plannedResources
}

// extractResourceID tries to extract resource ID from log lines
func extractResourceID(lines []string, address string) string {
	// Look for resource.id = "..." pattern in lines near the address
	resourceIDPattern := regexp.MustCompile(`resource\.id\s*=\s*"([^"]+)"`)

	// Search a few lines before and after (within context window)
	for i, line := range lines {
		if strings.Contains(line, address) {
			// Check surrounding lines
			start := max(0, i-5)
			end := min(len(lines), i+10)
			for j := start; j < end; j++ {
				if match := resourceIDPattern.FindStringSubmatch(lines[j]); match != nil {
					return match[1]
				}
			}
		}
	}
	return ""
}

// extractCreationTime tries to extract creation time from log line
func extractCreationTime(line string) *time.Time {
	// Look for time patterns in the line
	// Terraform logs often include timestamps like "after 2s" or ISO timestamps
	// For now, return nil - can be enhanced later if needed
	return nil
}

// Helper functions
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
