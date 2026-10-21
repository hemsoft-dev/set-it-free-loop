//go:build linux

package nativefixture

import (
	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	"strings"
	"testing"
)

func TestNativeControlBackendUsesExactFourStepLineage(t *testing.T) {
	for _, p := range []r.Provider{r.Codex, r.Claude} {
		cfg := r.DefaultConfig()
		cfg.Mode = r.CodexOnly
		if p == r.Claude {
			cfg.Mode = r.ClaudeOnly
		}
		cfg.Limits.OutputBytes = r.EvaluationOutputBytes
		snap := r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: 1, Head: head, Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("c", 40)}
		job := r.Job{ID: "fixture-control", RequesterID: r.OwnerID, Generation: 1, ConfigVersion: 1, Release: strings.Repeat("d", 40), Snapshot: snap, Config: cfg, Packet: "immutable source evidence"}
		input := r.ControlSession{Job: job, Provider: p, Step: 1, Limits: cfg.Limits, History: []r.ControlReport{}}
		for step := 1; step <= 4; step++ {
			backend, _, err := NewControl(input)
			if err != nil {
				t.Fatal(err)
			}
			report := sourceControlReport(input)
			if report.Step != step || r.ValidateControlReport(report, input) != nil {
				t.Fatal("invalid fixture producer", step)
			}
			if backend.Codex != nil && r.Digest(backend.Codex.output) != r.Digest(report) || backend.Claude != nil && r.Digest(backend.Claude.output) != r.Digest(report) {
				t.Fatal("backend selected peer output")
			}
			if step < 4 {
				input.History = append(input.History, report)
				input.Step++
			}
		}
	}
}
