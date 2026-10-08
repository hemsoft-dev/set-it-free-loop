package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
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
	if err := checkOnboardingInstallLink(string(doc), want); err != nil {
		t.Fatal(err)
	}
}

func checkOnboardingInstallLink(doc, want string) error {
	pattern := regexp.MustCompile(`(?m)^Install the \[checksum-verified canonical release\]\(([^)]+)\)\.`)
	matches := pattern.FindAllStringSubmatch(doc, -1)
	if len(matches) != 1 || matches[0][1] != want {
		return fmt.Errorf("designated onboarding install link must uniquely match distribution metadata: %s", want)
	}
	return nil
}

func TestOnboardingInstallLinkRejectsIncidentalOrDuplicateReleaseURLs(t *testing.T) {
	want := "https://github.com/hemsoft-dev/set-it-free-loop/releases/tag/v2.1.0-rc.21"
	correct := "Install the [checksum-verified canonical release](" + want + ")."
	for name, doc := range map[string]string{
		"history only":                           "Release history: [latest](" + want + ")",
		"stale instruction with current history": "Install the [checksum-verified canonical release](https://example.com/stale).\n" + want,
		"duplicate instruction":                  correct + "\n" + correct,
	} {
		t.Run(name, func(t *testing.T) {
			if checkOnboardingInstallLink(doc, want) == nil {
				t.Fatal("invalid installation instruction was accepted")
			}
		})
	}
	if err := checkOnboardingInstallLink(correct, want); err != nil {
		t.Fatal(err)
	}
}
