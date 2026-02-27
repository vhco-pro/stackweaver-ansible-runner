// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package plugins

import (
	"context"
)

type Plugin interface {
	Init(ctx context.Context, workspaceDir string, config map[string]interface{}, envVars map[string]string) (*CommandResult, error)
	Plan(ctx context.Context, workspaceDir string, variables map[string]string, envVars map[string]string) (*PlanResult, error)
	Apply(ctx context.Context, workspaceDir string, planFile string, envVars map[string]string) (*CommandResult, error)
	Destroy(ctx context.Context, workspaceDir string, envVars map[string]string) (*CommandResult, error)
	Validate(ctx context.Context, workspaceDir string, envVars map[string]string) error
}

type CommandResult struct {
	Output string // Combined stdout and stderr
	Logs   string // Full logs (same as Output for now, but can be extended)
}

type PlanResult struct {
	AddCount          int
	ChangeCount       int
	DestroyCount      int
	OutputChangeCount int
	Output            string
	JSONOutput        map[string]interface{}
	Logs              string // Full logs from terraform plan
}

type RunContext struct {
	WorkspaceID string
	RunID       string
	Variables   map[string]string
	Config      map[string]interface{}
}
