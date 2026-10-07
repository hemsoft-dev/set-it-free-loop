package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// sflManifest represents the .sfl/sfl.json file in a consumer repo.
type sflManifest struct {
	Version      string                   `json:"version"`
	Tier         string                   `json:"tier"`
	MotherRepo   string                   `json:"source"`
	DeployedAt   time.Time                `json:"deployedAt"`
	DeployedBy   string                   `json:"deployedBy"`
	SourceSHA    string                   `json:"sourceSha,omitempty"`
	Components   []string                 `json:"components"`
	Addons       []string                 `json:"addons,omitempty"`
	EnginePolicy *sflEnginePolicyManifest `json:"enginePolicy,omitempty"`
}

type sflEnginePolicyManifest struct {
	DefaultProfile string                     `json:"defaultProfile"`
	Workflows      []sflEngineWorkflowProfile `json:"workflows"`
}

type sflEngineWorkflowProfile struct {
	Name                 string            `json:"name"`
	Profile              string            `json:"profile"`
	Provider             string            `json:"provider"`
	Model                string            `json:"model"`
	Effort               string            `json:"effort,omitempty"`
	RenderedModel        string            `json:"renderedModel"`
	RequiredSecretsAnyOf []string          `json:"requiredSecretsAnyOf,omitempty"`
	Arguments            []string          `json:"arguments,omitempty"`
	Environment          map[string]string `json:"environment,omitempty"`
}

func (m *sflManifest) UnmarshalJSON(data []byte) error {
	type manifestWire struct {
		Version          string                   `json:"version"`
		Tier             string                   `json:"tier"`
		Source           string                   `json:"source"`
		LegacyMotherRepo string                   `json:"motherRepo"`
		DeployedAt       time.Time                `json:"deployedAt"`
		DeployedBy       string                   `json:"deployedBy"`
		SourceSHA        string                   `json:"sourceSha"`
		LegacySourceSHA  string                   `json:"sourceSHA"`
		Components       []string                 `json:"components"`
		Addons           []string                 `json:"addons"`
		EnginePolicy     *sflEnginePolicyManifest `json:"enginePolicy"`
	}
	var wire manifestWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	m.Version = wire.Version
	m.Tier = wire.Tier
	m.MotherRepo = wire.Source
	if m.MotherRepo == "" {
		m.MotherRepo = wire.LegacyMotherRepo
	}
	m.DeployedAt = wire.DeployedAt
	m.DeployedBy = wire.DeployedBy
	m.SourceSHA = wire.SourceSHA
	if m.SourceSHA == "" {
		m.SourceSHA = wire.LegacySourceSHA
	}
	m.Components = wire.Components
	m.Addons = wire.Addons
	m.EnginePolicy = wire.EnginePolicy
	return nil
}

// readLocalManifest reads .sfl/sfl.json from the current directory.
func readLocalManifest() (*sflManifest, error) {
	data, err := os.ReadFile(filepath.Join(".sfl", "sfl.json"))
	if err != nil {
		return nil, err
	}
	var m sflManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing .sfl/sfl.json: %w", err)
	}
	return &m, nil
}

// readRemoteManifest reads .sfl/sfl.json from a remote repo.
func readRemoteManifest(owner, repo string) (*sflManifest, error) {
	return readRemoteManifestWithFetcher(owner, repo, fetchFileRaw)
}

// Prefer the canonical manifest, but retain legacy root deployments. Failed or
// malformed canonical reads must never fall back to stale root configuration.
func readRemoteManifestWithFetcher(owner, repo string, fetch func(string, string, string, string) (string, error)) (*sflManifest, error) {
	manifestPath := ".sfl/sfl.json"
	raw, err := fetch(owner, repo, manifestPath, "")
	if err != nil && isNotFoundError(err) {
		manifestPath = "sfl.json"
		raw, err = fetch(owner, repo, manifestPath, "")
	}
	if err != nil {
		return nil, err
	}
	var m sflManifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("parsing remote %s: %w", manifestPath, err)
	}
	return &m, nil
}

// marshalManifest serializes a manifest to pretty-printed JSON.
func marshalManifest(m *sflManifest) (string, error) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data) + "\n", nil
}
