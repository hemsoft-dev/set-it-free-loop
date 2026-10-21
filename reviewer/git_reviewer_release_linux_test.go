//go:build linux

package reviewer

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestGitReviewerReleaseCanonicalPackageBindsAllPins(t *testing.T) {
	o, _, _, _ := gitReviewerStartupFixture(t)
	r := o.Releases[o.Current]
	raw, hash, err := EncodeGitReviewerRelease(r)
	if err != nil {
		t.Fatal(err)
	}
	decoded, next, err := DecodeGitReviewerRelease(raw)
	if err != nil || hash != next || next != gitRawHash(raw) {
		t.Fatal("canonical package did not round trip", err)
	}
	again, _, _ := EncodeGitReviewerRelease(decoded)
	if !bytes.Equal(raw, again) {
		t.Fatal("package regeneration changed bytes")
	}
	p := decoded.Providers[Codex]
	p.Runtime.BootstrapLibraries[0].SHA256 = strings.Repeat("e", 64)
	decoded.Providers[Codex] = p
	_, changed, err := EncodeGitReviewerRelease(decoded)
	if err != nil || changed == hash {
		t.Fatal("provider bootstrap pins absent from release digest", err)
	}
	decoded.Revision = strings.Repeat("f", 40)
	_, revision, _ := EncodeGitReviewerRelease(decoded)
	if revision == changed {
		t.Fatal("release revision absent from digest")
	}
}

func TestGitReviewerReleaseRejectsMalformedAndMismatchedDescriptors(t *testing.T) {
	o, _, _, _ := gitReviewerStartupFixture(t)
	raw, _, err := EncodeGitReviewerRelease(o.Releases[o.Current])
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"unknown", "duplicate", "trailing", "null-providers", "missing-version", "wrong-artifact", "wrong-identity", "missing-bootstrap", "wrong-response"} {
		t.Run(mutation, func(t *testing.T) {
			bad := append([]byte{}, raw...)
			var doc map[string]any
			json.Unmarshal(bad, &doc)
			switch mutation {
			case "unknown":
				doc["Unknown"] = true
			case "duplicate":
				bad = append([]byte(`{"Version":1,`), bad[1:]...)
			case "trailing":
				bad = append(bad, []byte(` {}`)...)
			case "null-providers":
				doc["Providers"] = nil
			case "missing-version":
				delete(doc, "Version")
			case "wrong-artifact":
				doc["GitArtifact"].(map[string]any)["SHA256"] = strings.Repeat("e", 64)
			case "wrong-identity":
				doc["Providers"].(map[string]any)[string(Codex)].(map[string]any)["Identity"].(map[string]any)["model"] = "unqualified-model"
			case "missing-bootstrap":
				doc["Providers"].(map[string]any)[string(Codex)].(map[string]any)["Runtime"].(map[string]any)["BootstrapLibraries"] = []any{}
			case "wrong-response":
				doc["Providers"].(map[string]any)[string(Codex)].(map[string]any)["Policy"].(map[string]any)["response_policy"] = ""
			}
			if mutation != "duplicate" && mutation != "trailing" {
				bad, _ = json.Marshal(doc)
			}
			if _, hash, err := DecodeGitReviewerRelease(bad); err == nil || hash != "" {
				t.Fatal("invalid package accepted")
			}
		})
	}
}

func TestGitReviewerReleaseRejectsNoncanonicalPackageBytes(t *testing.T) {
	o, _, _, _ := gitReviewerStartupFixture(t)
	raw, _, err := EncodeGitReviewerRelease(o.Releases[o.Current])
	if err != nil {
		t.Fatal(err)
	}
	var reordered map[string]any
	if json.Unmarshal(raw, &reordered) != nil {
		t.Fatal("fixture")
	}
	otherOrder, _ := json.Marshal(reordered)
	for _, changed := range [][]byte{append([]byte(" "), raw...), append(append([]byte{}, raw...), '\n'), otherOrder} {
		if bytes.Equal(raw, changed) {
			t.Fatal("fixture did not change package bytes")
		}
		if _, hash, err := DecodeGitReviewerRelease(changed); err == nil || hash != "" {
			t.Fatal("decoder approved a digest of different package bytes")
		}
	}
}
