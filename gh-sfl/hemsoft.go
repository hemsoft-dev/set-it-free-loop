package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

const hemSoftEnginePolicyJSON = "{\r\n  \"$schema\": \"./engine-policy.schema.json\",\r\n  \"defaultProfile\": \"codex-gpt-55-high\",\r\n  \"profiles\": {\r\n    \"codex-gpt-55-high\": {\r\n      \"provider\": \"codex\",\r\n      \"model\": \"gpt-5.5\",\r\n      \"effort\": \"high\",\r\n      \"requiredSecretsAnyOf\": [\r\n        \"CODEX_API_KEY\",\r\n        \"OPENAI_API_KEY\"\r\n      ]\r\n    }\r\n  },\r\n  \"workflows\": {}\r\n}\r\n"

type hemSoftEnginePolicy struct {
	DefaultProfile string                              `json:"defaultProfile"`
	Profiles       map[string]hemSoftEngineProfile     `json:"profiles"`
	Workflows      map[string]hemSoftWorkflowEngineRef `json:"workflows"`
}

type hemSoftEngineProfile struct {
	Provider             string            `json:"provider"`
	Model                string            `json:"model"`
	Effort               string            `json:"effort,omitempty"`
	RequiredSecretsAnyOf []string          `json:"requiredSecretsAnyOf,omitempty"`
	Arguments            []string          `json:"arguments,omitempty"`
	Environment          map[string]string `json:"environment,omitempty"`
}

type hemSoftWorkflowEngineRef struct {
	Profile string `json:"profile"`
}

type hemSoftEngineConfig struct {
	Profile              string
	Provider             string
	Model                string
	Effort               string
	RenderedModel        string
	RequiredSecretsAnyOf []string
	Arguments            []string
	Environment          map[string]string
}

var (
	hemSoftEnginePolicyOnce sync.Once
	hemSoftParsedPolicy     hemSoftEnginePolicy
	hemSoftPolicyError      error
)

func hemSoftEnginePolicyConfig() (*hemSoftEnginePolicy, error) {
	hemSoftEnginePolicyOnce.Do(func() {
		hemSoftPolicyError = json.Unmarshal([]byte(hemSoftEnginePolicyJSON), &hemSoftParsedPolicy)
		if hemSoftPolicyError != nil {
			hemSoftPolicyError = fmt.Errorf("parsing embedded HemSoft engine policy: %w", hemSoftPolicyError)
			return
		}
		if hemSoftParsedPolicy.DefaultProfile == "" {
			hemSoftPolicyError = fmt.Errorf("embedded HemSoft engine policy has no default profile")
		}
	})

	if hemSoftPolicyError != nil {
		return nil, hemSoftPolicyError
	}
	return &hemSoftParsedPolicy, nil
}

func hemSoftEngineConfigForWorkflow(workflowName string) (hemSoftEngineConfig, error) {
	policy, err := hemSoftEnginePolicyConfig()
	if err != nil {
		return hemSoftEngineConfig{}, err
	}

	profileName := policy.DefaultProfile
	if workflow, ok := policy.Workflows[workflowName]; ok && workflow.Profile != "" {
		profileName = workflow.Profile
	}

	profile, ok := policy.Profiles[profileName]
	if !ok {
		return hemSoftEngineConfig{}, fmt.Errorf(
			"engine profile %q for workflow %s was not found",
			profileName,
			workflowName,
		)
	}
	if profile.Provider == "" {
		return hemSoftEngineConfig{}, fmt.Errorf("engine profile %q has no provider", profileName)
	}
	if profile.Model == "" {
		return hemSoftEngineConfig{}, fmt.Errorf("engine profile %q has no model", profileName)
	}
	switch profile.Effort {
	case "", "low", "medium", "high":
	default:
		return hemSoftEngineConfig{}, fmt.Errorf(
			"engine profile %q uses unsupported effort %q",
			profileName,
			profile.Effort,
		)
	}

	renderedModel := profile.Model
	if profile.Effort != "" {
		renderedModel += "?effort=" + profile.Effort
	}

	return hemSoftEngineConfig{
		Profile:              profileName,
		Provider:             profile.Provider,
		Model:                profile.Model,
		Effort:               profile.Effort,
		RenderedModel:        renderedModel,
		RequiredSecretsAnyOf: append([]string(nil), profile.RequiredSecretsAnyOf...),
		Arguments:            append([]string(nil), profile.Arguments...),
		Environment:          cloneHemSoftEnvironment(profile.Environment),
	}, nil
}

func cloneHemSoftEnvironment(environment map[string]string) map[string]string {
	if len(environment) == 0 {
		return nil
	}

	cloned := make(map[string]string, len(environment))
	for key, value := range environment {
		cloned[key] = value
	}
	return cloned
}

func applyHemSoftOwnership(fileMap map[string]string) error {
	return applyHemSoftEnginePolicy(fileMap)
}

func applyHemSoftEnginePolicy(fileMap map[string]string) error {
	for fpath, content := range fileMap {
		if !strings.HasPrefix(fpath, ".github/workflows/") || !strings.HasSuffix(fpath, ".md") {
			continue
		}

		workflowName := strings.TrimSuffix(strings.TrimPrefix(fpath, ".github/workflows/"), ".md")
		rewritten, err := applyHemSoftEnginePolicyToWorkflow(content, workflowName)
		if err != nil {
			return err
		}

		fileMap[fpath] = rewritten
	}

	return nil
}

func applyHemSoftEnginePolicyToWorkflow(content, workflowName string) (string, error) {
	engineConfig, err := hemSoftEngineConfigForWorkflow(workflowName)
	if err != nil {
		return "", err
	}

	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return "", fmt.Errorf("workflow %s does not start with YAML frontmatter", workflowName)
	}

	frontmatterStart := len("---\n")
	closingOffset := strings.Index(normalized[frontmatterStart:], "\n---")
	if closingOffset < 0 {
		return "", fmt.Errorf("workflow %s has no closing YAML frontmatter marker", workflowName)
	}

	frontmatterEnd := frontmatterStart + closingOffset
	frontmatter := normalized[frontmatterStart:frontmatterEnd]
	rest := normalized[frontmatterEnd+len("\n---"):]

	frontmatter = removeTopLevelYamlEntry(frontmatter, "engine")
	frontmatter = removeTopLevelYamlEntry(frontmatter, "model")
	frontmatter = insertTopLevelEngineBlock(
		frontmatter,
		hemSoftEngineBlock(engineConfig)+"\n\nmodel: "+engineConfig.RenderedModel,
	)

	return "---\n" + frontmatter + "\n---" + rest, nil
}

func removeTopLevelYamlEntry(frontmatter, key string) string {
	lines := strings.Split(frontmatter, "\n")
	output := make([]string, 0, len(lines))
	skippingBlock := false
	skipFollowingBlank := false

	for _, line := range lines {
		if isTopLevelYamlKey(line, key) {
			for len(output) > 0 && strings.TrimSpace(output[len(output)-1]) == "" {
				output = output[:len(output)-1]
			}
			_, value, _ := strings.Cut(strings.TrimSpace(line), ":")
			value = strings.TrimSpace(value)
			skippingBlock = value == "" || strings.HasPrefix(value, "|") || strings.HasPrefix(value, ">")
			skipFollowingBlank = true
			continue
		}

		if skippingBlock {
			if strings.TrimSpace(line) == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
				continue
			}
			skippingBlock = false
		}
		if skipFollowingBlank && strings.TrimSpace(line) == "" {
			continue
		}
		skipFollowingBlank = false

		output = append(output, line)
	}

	return strings.TrimRight(strings.Join(output, "\n"), "\n")
}

func insertTopLevelEngineBlock(frontmatter, engineBlock string) string {
	lines := strings.Split(frontmatter, "\n")
	output := make([]string, 0, len(lines)+4)
	inserted := false

	for _, line := range lines {
		if !inserted && isTopLevelYamlKey(line, "network") {
			for len(output) > 0 && strings.TrimSpace(output[len(output)-1]) == "" {
				output = output[:len(output)-1]
			}
			if len(output) > 0 {
				output = append(output, "")
			}
			output = append(output, strings.Split(engineBlock, "\n")...)
			output = append(output, "")
			inserted = true
		}
		output = append(output, line)
	}

	if !inserted {
		if strings.TrimSpace(frontmatter) != "" {
			output = append(output, "")
		}
		output = append(output, strings.Split(engineBlock, "\n")...)
	}

	return strings.Join(output, "\n")
}

func isTopLevelYamlKey(line, key string) bool {
	if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
		return false
	}

	trimmed := strings.TrimSpace(line)
	return trimmed == key+":" || strings.HasPrefix(trimmed, key+": ")
}

func hemSoftEngineBlock(config hemSoftEngineConfig) string {
	lines := []string{"engine:", "  id: " + config.Provider}
	if len(config.Arguments) > 0 {
		lines = append(lines, "  args:")
		for _, argument := range config.Arguments {
			lines = append(lines, "    - '"+strings.ReplaceAll(argument, "'", "''")+"'")
		}
	}
	if len(config.Environment) > 0 {
		keys := make([]string, 0, len(config.Environment))
		for key := range config.Environment {
			keys = append(keys, key)
		}
		sort.Strings(keys)

		lines = append(lines, "  env:")
		for _, key := range keys {
			lines = append(lines, "    "+key+": "+config.Environment[key])
		}
	}
	return strings.Join(lines, "\n")
}

func hemSoftEnginePolicyManifestForFileMap(fileMap map[string]string) *sflEnginePolicyManifest {
	policy, err := hemSoftEnginePolicyConfig()
	if err != nil {
		panic(err)
	}

	workflowNames := make([]string, 0)
	for fpath := range fileMap {
		if !strings.HasPrefix(fpath, ".github/workflows/") || !strings.HasSuffix(fpath, ".md") {
			continue
		}

		workflowNames = append(
			workflowNames,
			strings.TrimSuffix(strings.TrimPrefix(fpath, ".github/workflows/"), ".md"),
		)
	}
	sort.Strings(workflowNames)

	workflows := make([]sflEngineWorkflowProfile, 0, len(workflowNames))
	for _, workflowName := range workflowNames {
		config, configErr := hemSoftEngineConfigForWorkflow(workflowName)
		if configErr != nil {
			panic(configErr)
		}

		workflows = append(workflows, sflEngineWorkflowProfile{
			Name:                 workflowName,
			Profile:              config.Profile,
			Provider:             config.Provider,
			Model:                config.Model,
			Effort:               config.Effort,
			RenderedModel:        config.RenderedModel,
			RequiredSecretsAnyOf: append([]string(nil), config.RequiredSecretsAnyOf...),
			Arguments:            append([]string(nil), config.Arguments...),
			Environment:          cloneHemSoftEnvironment(config.Environment),
		})
	}

	return &sflEnginePolicyManifest{
		DefaultProfile: policy.DefaultProfile,
		Workflows:      workflows,
	}
}

func mergeHemSoftEnginePolicyManifest(
	existing, additional *sflEnginePolicyManifest,
) *sflEnginePolicyManifest {
	if existing == nil {
		return additional
	}
	if additional == nil {
		return existing
	}

	byName := make(map[string]sflEngineWorkflowProfile, len(existing.Workflows)+len(additional.Workflows))
	for _, workflow := range existing.Workflows {
		byName[workflow.Name] = workflow
	}
	for _, workflow := range additional.Workflows {
		byName[workflow.Name] = workflow
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	workflows := make([]sflEngineWorkflowProfile, 0, len(names))
	for _, name := range names {
		workflows = append(workflows, byName[name])
	}
	defaultProfile := additional.DefaultProfile
	if defaultProfile == "" {
		defaultProfile = existing.DefaultProfile
	}
	return &sflEnginePolicyManifest{
		DefaultProfile: defaultProfile,
		Workflows:      workflows,
	}
}

func sourceWorkflowPath(name string) string {
	switch name {
	case "sfl-dispatcher.yml", "sfl-auditor.yml", "sfl-pr-review-auto.yml":
		return "deployment/infrastructure/" + name
	default:
		return "deployment/workflows/" + name
	}
}

const reviewerSourcePlaceholder = "# Source: HemSoft/set-it-free-loop/deployment/infrastructure/sfl-pr-review-auto.yml@main"

func prepareWorkflowSource(workflow, content, sourceSHA, targetRepo string) (string, error) {
	if workflow != "sfl-pr-review-auto.yml" {
		return content, nil
	}
	if !strings.Contains(content, "name: SFL Codex Review Observer") ||
		!strings.Contains(content, "github.event.sender.id == 199175422") ||
		!strings.Contains(content, reviewerSourcePlaceholder) {
		return "", fmt.Errorf("the SFL source %s selected for %s predates the subscription-backed Codex reviewer; deploy or sync from a release containing SFL Codex Review Observer", sourceSHA, targetRepo)
	}
	sourceRef := "HemSoft/set-it-free-loop/" + sourceWorkflowPath(workflow) + "@" + sourceSHA
	content = strings.Replace(content, reviewerSourcePlaceholder, "# Source: "+sourceRef, 1)
	prefix := "# Deployed from: " + sourceRef + "\n" +
		"# To upgrade: re-run deploy-workflow.ps1 at the desired SHA\n"
	return prefix + content, nil
}

func renderHemSoftWorkflow(name, content, sflVersion string) (string, error) {
	rendered := renderWorkflow(content, sflVersion)
	if !strings.HasSuffix(name, ".md") {
		return rendered, nil
	}
	return applyHemSoftEnginePolicyToWorkflow(
		rendered,
		strings.TrimSuffix(name, ".md"),
	)
}
