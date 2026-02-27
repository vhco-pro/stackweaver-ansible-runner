// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package registry

import (
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// ModuleMetadata contains parsed metadata from a Terraform module
type ModuleMetadata struct {
	Inputs       []InputDefinition
	Outputs      []OutputDefinition
	Dependencies []ProviderDependency
	Resources    []ResourceType
	Submodules   []SubmoduleInfo
	Readme       string
}

// InputDefinition represents a variable definition
type InputDefinition struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Type        string      `json:"type,omitempty"`
	Default     interface{} `json:"default,omitempty"`
	Required    bool        `json:"required"`
}

// OutputDefinition represents an output definition
type OutputDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ProviderDependency represents a required provider
type ProviderDependency struct {
	Name    string `json:"name"`
	Source  string `json:"source"`
	Version string `json:"version,omitempty"`
}

// ResourceType represents a Terraform resource type used in the module
type ResourceType struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// SubmoduleInfo represents a submodule within the module
type SubmoduleInfo struct {
	Path    string             `json:"path"`
	Readme  string             `json:"readme,omitempty"`
	Empty   bool               `json:"empty"`
	Inputs  []InputDefinition  `json:"inputs,omitempty"`
	Outputs []OutputDefinition `json:"outputs,omitempty"`
}

// ModuleParser parses Terraform modules to extract metadata
type ModuleParser struct {
	parser *hclparse.Parser
}

// NewModuleParser creates a new module parser
func NewModuleParser() *ModuleParser {
	return &ModuleParser{
		parser: hclparse.NewParser(),
	}
}

// ParseModule parses a Terraform module directory and extracts metadata
func (p *ModuleParser) ParseModule(dir string) (*ModuleMetadata, error) {
	metadata := &ModuleMetadata{
		Inputs:       []InputDefinition{},
		Outputs:      []OutputDefinition{},
		Dependencies: []ProviderDependency{},
		Resources:    []ResourceType{},
		Submodules:   []SubmoduleInfo{},
	}

	// Parse variables.tf
	variablesPath := filepath.Join(dir, "variables.tf")
	if _, err := os.Stat(variablesPath); err == nil {
		inputs, err := p.parseVariables(variablesPath)
		if err != nil {
			return nil, fmt.Errorf("failed to parse variables: %w", err)
		}
		metadata.Inputs = inputs
	}

	// Parse outputs.tf
	outputsPath := filepath.Join(dir, "outputs.tf")
	if _, err := os.Stat(outputsPath); err == nil {
		outputs, err := p.parseOutputs(outputsPath)
		if err != nil {
			return nil, fmt.Errorf("failed to parse outputs: %w", err)
		}
		metadata.Outputs = outputs
	}

	// Parse versions.tf or terraform.tf for dependencies
	versionsPath := filepath.Join(dir, "versions.tf")
	if _, err := os.Stat(versionsPath); err != nil {
		versionsPath = filepath.Join(dir, "terraform.tf")
	}
	if _, err := os.Stat(versionsPath); err == nil {
		deps, err := p.parseDependencies(versionsPath)
		if err != nil {
			return nil, fmt.Errorf("failed to parse dependencies: %w", err)
		}
		metadata.Dependencies = deps
	}

	// Parse all .tf files for resources
	resources, err := p.parseResources(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to parse resources: %w", err)
	}
	metadata.Resources = resources

	// Detect submodules
	submodules, err := p.detectSubmodules(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to detect submodules: %w", err)
	}
	metadata.Submodules = submodules

	// Read README
	readmePath := filepath.Join(dir, "README.md")
	if _, err := os.Stat(readmePath); err == nil {
		readme, err := os.ReadFile(readmePath) //nolint:gosec // readmePath is validated (from module directory)
		if err == nil {
			metadata.Readme = string(readme)
		}
	}

	return metadata, nil
}

// parseVariables parses variables.tf file
func (p *ModuleParser) parseVariables(path string) ([]InputDefinition, error) {
	file, diags := p.parser.ParseHCLFile(path)
	if diags.HasErrors() {
		return nil, fmt.Errorf("parse error: %s", diags.Error())
	}

	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return nil, fmt.Errorf("unexpected body type")
	}

	// Read file bytes for source extraction fallback
	fileBytes, err := os.ReadFile(path) //nolint:gosec // path is validated (from module directory)
	if err != nil {
		fileBytes = nil // Continue without source extraction
	}

	var inputs []InputDefinition

	// Find all variable blocks
	for _, block := range body.Blocks {
		if block.Type == "variable" && len(block.Labels) > 0 {
			varName := block.Labels[0]
			input := InputDefinition{
				Name:     varName,
				Required: true, // Default to required
			}

			// Parse variable attributes
			if block.Body != nil {
				for name, attr := range block.Body.Attributes {
					switch name {
					case "description":
						val, _ := attr.Expr.Value(nil)
						if val.Type() == cty.String {
							input.Description = val.AsString()
						}
					case "type":
						// Extract full type string representation recursively
						input.Type = p.extractTypeString(attr.Expr, fileBytes)
					case "default":
						val, _ := attr.Expr.Value(nil)
						if val != cty.NilVal {
							input.Default = p.ctyValueToInterface(val)
							input.Required = false
						}
					}
				}
			}

			inputs = append(inputs, input)
		}
	}

	return inputs, nil
}

// extractTypeString recursively extracts the full type string from an HCL expression
// fileBytes is used as a fallback to extract raw source when expression parsing fails
func (p *ModuleParser) extractTypeString(expr hclsyntax.Expression, fileBytes []byte) string {
	switch e := expr.(type) {
	case *hclsyntax.ScopeTraversalExpr:
		// Simple type reference like `string`, `number`, etc.
		if len(e.Traversal) > 0 {
			return e.Traversal.RootName()
		}
		return "any"
	case *hclsyntax.FunctionCallExpr:
		// Type constructor like `list(string)`, `map(string)`, `object({...})`, etc.
		if len(e.Args) == 0 {
			return e.Name
		}

		// Recursively extract argument types
		var argStrs []string
		for _, arg := range e.Args {
			argStrs = append(argStrs, p.extractTypeString(arg, fileBytes))
		}

		if len(argStrs) == 1 {
			return fmt.Sprintf("%s(%s)", e.Name, argStrs[0])
		}
		return fmt.Sprintf("%s(%s)", e.Name, strings.Join(argStrs, ", "))
	case *hclsyntax.ObjectConsExpr:
		// Object type definition like `{ key = type }` (the inner part of object({...}))
		if len(e.Items) == 0 {
			return "{}"
		}

		var items []string
		for _, item := range e.Items {
			// Extract key - most common case is identifier (ScopeTraversalExpr)
			keyStr := "?"
			if item.KeyExpr != nil {
				if keyTraversal, ok := item.KeyExpr.(*hclsyntax.ScopeTraversalExpr); ok && len(keyTraversal.Traversal) > 0 {
					keyStr = keyTraversal.Traversal.RootName()
				} else if keyVal, diags := item.KeyExpr.Value(nil); !diags.HasErrors() && keyVal.Type() == cty.String {
					keyStr = keyVal.AsString()
				}
			}

			// Extract value type
			valStr := p.extractTypeString(item.ValueExpr, fileBytes)

			// Handle optional() wrapper
			if valCall, ok := item.ValueExpr.(*hclsyntax.FunctionCallExpr); ok && valCall.Name == "optional" && len(valCall.Args) > 0 {
				valStr = fmt.Sprintf("optional(%s)", p.extractTypeString(valCall.Args[0], fileBytes))
			}

			items = append(items, fmt.Sprintf("%s = %s", keyStr, valStr))
		}

		return fmt.Sprintf("{ %s }", strings.Join(items, " "))
	case *hclsyntax.TupleConsExpr:
		// Tuple type definition
		if len(e.Exprs) == 0 {
			return "tuple([])"
		}

		var typeStrs []string
		for _, expr := range e.Exprs {
			typeStrs = append(typeStrs, p.extractTypeString(expr, fileBytes))
		}
		return fmt.Sprintf("tuple([%s])", strings.Join(typeStrs, ", "))
	default:
		// For unknown expression types, extract from source as fallback
		if fileBytes != nil {
			if rangeExpr, ok := expr.(hclsyntax.Node); ok {
				rng := rangeExpr.Range()
				if rng.Start.Byte >= 0 && rng.End.Byte > rng.Start.Byte && rng.End.Byte <= len(fileBytes) {
					return strings.TrimSpace(string(fileBytes[rng.Start.Byte:rng.End.Byte]))
				}
			}
		}
		return "any"
	}
}

// ctyValueToInterface converts a cty.Value to a Go interface{}
func (p *ModuleParser) ctyValueToInterface(val cty.Value) interface{} {
	// Handle null values first
	if val.IsNull() {
		return nil
	}

	if !val.IsKnown() {
		return nil
	}

	ty := val.Type()

	switch {
	case ty == cty.String:
		return val.AsString()
	case ty == cty.Number:
		// Try as int first, then float
		bf := val.AsBigFloat()
		if bf.IsInt() {
			if i, acc := bf.Int64(); acc == big.Exact {
				return i
			}
		}
		f, _ := bf.Float64()
		return f
	case ty == cty.Bool:
		return val.True()
	case ty.IsListType() || ty.IsSetType():
		// Handle lists and sets of any element type recursively
		var result []interface{}
		for it := val.ElementIterator(); it.Next(); {
			_, v := it.Element()
			result = append(result, p.ctyValueToInterface(v))
		}
		return result
	case ty.IsMapType():
		// Handle maps of any value type recursively
		result := make(map[string]interface{})
		for it := val.ElementIterator(); it.Next(); {
			k, v := it.Element()
			result[k.AsString()] = p.ctyValueToInterface(v)
		}
		return result
	case ty.IsObjectType():
		// Handle objects (complex types with named attributes) recursively
		result := make(map[string]interface{})
		for k := range ty.AttributeTypes() {
			attrVal := val.GetAttr(k)
			result[k] = p.ctyValueToInterface(attrVal)
		}
		return result
	case ty.IsTupleType():
		// Handle tuples (lists with specific element types) recursively
		var result []interface{}
		for it := val.ElementIterator(); it.Next(); {
			_, v := it.Element()
			result = append(result, p.ctyValueToInterface(v))
		}
		return result
	default:
		// For truly unknown types, return nil instead of GoString()
		// This prevents showing "cty.NullVal(...)" or "cty.Objectval(...)"
		return nil
	}
}

// parseOutputs parses outputs.tf file
func (p *ModuleParser) parseOutputs(path string) ([]OutputDefinition, error) {
	file, diags := p.parser.ParseHCLFile(path)
	if diags.HasErrors() {
		return nil, fmt.Errorf("parse error: %s", diags.Error())
	}

	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return nil, fmt.Errorf("unexpected body type")
	}

	var outputs []OutputDefinition

	// Find all output blocks
	for _, block := range body.Blocks {
		if block.Type == "output" && len(block.Labels) > 0 {
			outputName := block.Labels[0]
			output := OutputDefinition{
				Name: outputName,
			}

			// Parse output attributes
			if block.Body != nil {
				for name, attr := range block.Body.Attributes {
					if name == "description" {
						val, _ := attr.Expr.Value(nil)
						if val.Type() == cty.String {
							output.Description = val.AsString()
						}
					}
				}
			}

			outputs = append(outputs, output)
		}
	}

	return outputs, nil
}

// parseDependencies parses versions.tf or terraform.tf for provider requirements
func (p *ModuleParser) parseDependencies(path string) ([]ProviderDependency, error) {
	file, diags := p.parser.ParseHCLFile(path)
	if diags.HasErrors() {
		return nil, fmt.Errorf("parse error: %s", diags.Error())
	}

	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return nil, fmt.Errorf("unexpected body type")
	}

	var deps []ProviderDependency

	// Find terraform block with required_providers
	for _, block := range body.Blocks {
		if block.Type == "terraform" && block.Body != nil {
			// Look for required_providers attribute (it's an attribute, not a block)
			if attr, ok := block.Body.Attributes["required_providers"]; ok {
				// Parse provider requirements
				val, _ := attr.Expr.Value(nil)
				if val.Type().IsObjectType() {
					for k, v := range val.AsValueMap() {
						dep := ProviderDependency{
							Name:   k,
							Source: "", // Will be extracted from the value
						}

						// Extract source and version from the provider value
						if v.Type().IsObjectType() {
							for propName, propVal := range v.AsValueMap() {
								switch propName {
								case "source":
									if propVal.Type() == cty.String {
										dep.Source = propVal.AsString()
									}
								case "version":
									if propVal.Type() == cty.String {
										dep.Version = propVal.AsString()
									}
								}
							}
						} else if v.Type() == cty.String {
							// Simple string value (version constraint)
							dep.Version = v.AsString()
						}

						// If source is empty, use default (hashicorp/provider-name)
						if dep.Source == "" {
							dep.Source = fmt.Sprintf("hashicorp/%s", dep.Name)
						}

						deps = append(deps, dep)
					}
				}
			}
		}
	}

	return deps, nil
}

// parseResources parses all .tf files to extract resource types
func (p *ModuleParser) parseResources(dir string) ([]ResourceType, error) {
	var resources []ResourceType

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Skip hidden directories and files
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip submodules directory (will be parsed separately)
		if d.IsDir() && d.Name() == "modules" {
			return filepath.SkipDir
		}

		// Only parse .tf files
		if !d.IsDir() && strings.HasSuffix(path, ".tf") {
			file, diags := p.parser.ParseHCLFile(path)
			if diags.HasErrors() {
				// Log but don't fail on parse errors
				return nil
			}

			body, ok := file.Body.(*hclsyntax.Body)
			if !ok {
				return nil
			}

			// Extract resource blocks
			for _, block := range body.Blocks {
				if block.Type == "resource" && len(block.Labels) >= 2 {
					resourceType := block.Labels[0]
					resourceName := block.Labels[1]
					resources = append(resources, ResourceType{
						Name: resourceName,
						Type: resourceType,
					})
				}
			}
		}

		return nil
	})

	return resources, err
}

// detectSubmodules detects submodules in the modules/ directory
func (p *ModuleParser) detectSubmodules(dir string) ([]SubmoduleInfo, error) {
	modulesDir := filepath.Join(dir, "modules")
	if _, err := os.Stat(modulesDir); os.IsNotExist(err) {
		return []SubmoduleInfo{}, nil
	}

	var submodules []SubmoduleInfo

	err := filepath.WalkDir(modulesDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.IsDir() {
			return nil
		}

		// Check if this directory contains .tf files (is a submodule)
		hasTerraformFiles := false
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil
		}

		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".tf") {
				hasTerraformFiles = true
				break
			}
		}

		if hasTerraformFiles {
			relPath, _ := filepath.Rel(modulesDir, path)
			submodules = append(submodules, SubmoduleInfo{
				Path:  filepath.Join("modules", relPath),
				Empty: false,
			})
		}

		return nil
	})

	return submodules, err
}

// ConvertToJSONB converts ModuleMetadata to models.JSONB format for storage
func (m *ModuleMetadata) ConvertToJSONB() map[string]interface{} {
	inputsJSON := make([]map[string]interface{}, len(m.Inputs))
	for i, input := range m.Inputs {
		inputsJSON[i] = map[string]interface{}{
			"name":        input.Name,
			"description": input.Description,
			"type":        input.Type,
			"default":     input.Default,
			"required":    input.Required,
		}
	}

	outputsJSON := make([]map[string]interface{}, len(m.Outputs))
	for i, output := range m.Outputs {
		outputsJSON[i] = map[string]interface{}{
			"name":        output.Name,
			"description": output.Description,
		}
	}

	depsJSON := make([]map[string]interface{}, len(m.Dependencies))
	for i, dep := range m.Dependencies {
		depsJSON[i] = map[string]interface{}{
			"name":    dep.Name,
			"source":  dep.Source,
			"version": dep.Version,
		}
	}

	resourcesJSON := make([]map[string]interface{}, len(m.Resources))
	for i, res := range m.Resources {
		resourcesJSON[i] = map[string]interface{}{
			"name": res.Name,
			"type": res.Type,
		}
	}

	submodulesJSON := make([]map[string]interface{}, len(m.Submodules))
	for i, submod := range m.Submodules {
		submodulesJSON[i] = map[string]interface{}{
			"path":   submod.Path,
			"readme": submod.Readme,
			"empty":  submod.Empty,
		}
	}

	return map[string]interface{}{
		"inputs":       inputsJSON,
		"outputs":      outputsJSON,
		"dependencies": depsJSON,
		"resources":    resourcesJSON,
		"submodules":   submodulesJSON,
	}
}
