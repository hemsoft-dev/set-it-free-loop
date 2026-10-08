package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
)

// Consumer-owned workflow files stay outside sync's write set. The required
// native reviewer cannot be excluded through this policy.
type consumerSyncPolicy struct {
	Version            int      `json:"version"`
	UnmanagedWorkflows []string `json:"unmanagedWorkflows"`
}

func parseConsumerSyncPolicy(data []byte) (*consumerSyncPolicy, error) {
	var policy consumerSyncPolicy
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return nil, fmt.Errorf("invalid consumer sync policy: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("consumer sync policy must contain one JSON object")
	}
	if policy.Version != 1 {
		return nil, fmt.Errorf("unsupported consumer sync policy version")
	}
	allowed := map[string]bool{}
	for _, name := range tierWorkflows["full"] {
		if name != "sfl-pr-review-auto.yml" {
			allowed[name] = true
		}
	}
	seen := map[string]bool{}
	for _, name := range policy.UnmanagedWorkflows {
		if !allowed[name] || seen[name] {
			return nil, fmt.Errorf("consumer sync policy has an unsupported or repeated workflow %q", name)
		}
		seen[name] = true
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("consumer sync policy must name consumer-owned workflows")
	}
	return &policy, nil
}

func readConsumerSyncPolicy(client restAPI, owner, repo, revision string) (*consumerSyncPolicy, error) {
	var response struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	path := fmt.Sprintf("repos/%s/%s/contents/.sfl/sync-policy.json?ref=%s", owner, repo, revision)
	if err := client.Get(path, &response); err != nil {
		var httpError *api.HTTPError
		if errors.As(err, &httpError) && httpError.StatusCode == 404 {
			return nil, nil
		}
		return nil, fmt.Errorf("reading consumer sync policy: %w", err)
	}
	if response.Encoding != "base64" {
		return nil, fmt.Errorf("unsupported consumer sync policy encoding")
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(response.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("decoding consumer sync policy: %w", err)
	}
	return parseConsumerSyncPolicy(data)
}

func applyConsumerSyncPolicy(files map[string]string, policy *consumerSyncPolicy) {
	if policy == nil {
		return
	}
	for _, name := range policy.UnmanagedWorkflows {
		delete(files, ".github/workflows/"+name)
	}
}

// Preserve recorded engine choices for consumer-owned workflows. These profiles
// describe configuration, and do not establish that a retired workflow exists.
func preserveConsumerEngineProfiles(next, previous *sflEnginePolicyManifest, policy *consumerSyncPolicy) {
	if next == nil || previous == nil || policy == nil {
		return
	}
	names := map[string]bool{}
	for _, name := range policy.UnmanagedWorkflows {
		names[strings.TrimSuffix(name, ".md")] = true
	}
	for _, profile := range previous.Workflows {
		if names[profile.Name] {
			next.Workflows = append(next.Workflows, profile)
		}
	}
}

func consumerPreservedPaths(policy *consumerSyncPolicy) []string {
	if policy == nil {
		return nil
	}
	var paths []string
	for _, name := range policy.UnmanagedWorkflows {
		paths = append(paths, ".github/workflows/"+name)
	}
	return paths
}

func consumerOwnsWorkflow(policy *consumerSyncPolicy, name string) bool {
	if policy == nil {
		return false
	}
	for _, owned := range policy.UnmanagedWorkflows {
		if owned == name {
			return true
		}
	}
	return false
}
