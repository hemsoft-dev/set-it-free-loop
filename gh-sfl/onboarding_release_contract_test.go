package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The release metadata and operator's install link must move together. A stale
// link can deploy an older observer even when the current CLI version is newer.
func TestOnboardingReleaseLinkMatchesDistribution(t *testing.T) {
	raw, err := os.ReadFile("../deployment/release-metadata.json")
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		Distribution struct {
			Repository string `json:"repository"`
			Tag        string `json:"tag"`
		} `json:"distribution"`
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Distribution.Repository == "" || metadata.Distribution.Tag == "" {
		t.Fatal("distribution repository and tag are required")
	}
	doc, err := os.ReadFile("../docs/ORGANIZATION-ONBOARDING.md")
	if err != nil {
		t.Fatal(err)
	}
	want := "https://github.com/" + metadata.Distribution.Repository + "/releases/tag/" + metadata.Distribution.Tag
	if !strings.Contains(string(doc), "("+want+")") {
		t.Fatalf("onboarding install link must match distribution metadata: %s", want)
	}
}
