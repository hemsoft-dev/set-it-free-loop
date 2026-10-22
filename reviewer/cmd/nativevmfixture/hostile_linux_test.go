//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
)

func TestHostileCaseCannotMixModesOrProviders(t *testing.T) {
	for _, c := range []config{{Provider: r.Codex, Hostile: "codex-shell"}, {Provider: r.Claude, Hostile: "claude-source-mcp"}} {
		if !c.validCase() {
			t.Fatal("hostile case rejected", c)
		}
	}
	for _, c := range []config{{Provider: r.Claude, Hostile: "codex-shell"}, {Provider: r.Codex, Hostile: "codex-unknown"}, {Hostile: "codex-shell", ProtocolRounds: 1}, {Provider: r.Codex, Hostile: "codex-shell", Negative: true}, {Provider: r.Codex, Hostile: "codex-shell", Refresh: true}, {Provider: r.Codex, Hostile: "codex-shell", Cancellation: "reader"}, {Purpose: "native-control-fake-only", Provider: r.Codex, Hostile: "codex-shell"}} {
		if c.validCase() {
			t.Fatal("mixed hostile case accepted", c)
		}
	}
}

// Captured from pinned Codex 0.163.0 and its code host inside the native VM.
func TestCapturedCodeModeProbeIsContainedAndMutationsAreNot(t *testing.T) {
	raw, err := os.ReadFile("testdata/native-code-probe.json")
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]string
	if json.Unmarshal(raw, &probe) != nil || !probeContained(probe) {
		t.Fatal("captured contained probe rejected")
	}
	for key, value := range map[string]string{"typeof:require": "function", "typeof:process": "object", "import:node:fs": "loaded:12", "fetch:http://10.0.0.1/": "status:200", "tool:shell": "function", "tools": "mcp__sfl_source__read,mcp__sfl_source__search,shell", "env": "PATH", "image:profile": "returned:undefined", "load:profile": `returned:"secret"`} {
		changed := map[string]string{}
		for k, v := range probe {
			changed[k] = v
		}
		changed[key] = value
		if probeContained(changed) {
			t.Fatal("capability accepted", key, value)
		}
	}
	missing := map[string]string{}
	for k, v := range probe {
		if k != "import:fs" {
			missing[k] = v
		}
	}
	if probeContained(missing) || probeContained(nil) {
		t.Fatal("incomplete probe accepted")
	}
}

func TestHostileOutcomeHelpers(t *testing.T) {
	head := strings.Repeat("a", 40)
	if !hostAnchorOnly(nil, head) || !hostAnchorOnly([]string{head + ":source.go"}, head) || hostAnchorOnly([]string{head + ":inject.md"}, head) || hostAnchorOnly([]string{head + ":source.go", head + ":source.go"}, head) {
		t.Fatal("host anchor audit differs")
	}
	for err, want := range map[error]string{nil: "none", errors.Join(errors.New("x"), r.ErrWorkerStopPending): "stop-pending", errors.Join(errors.New("x"), r.ErrAuthRecoveryRequired): "auth-recovery", context.Canceled: "deadline-or-cancel", r.PhaseError{State: "invalid_output"}: "phase-invalid_output", errors.New("x"): "other"} {
		if got := errorClass(err); got != want {
			t.Fatal("error class differs", err, got)
		}
	}
}
