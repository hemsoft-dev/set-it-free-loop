//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	f "github.com/hemsoft-dev/set-it-free-loop/reviewer/internal/nativefixture"
)

func allPass(checks map[string]bool) bool {
	for _, ok := range checks {
		if !ok {
			return false
		}
	}
	return len(checks) > 2
}

func exhaustionBase(name string) exhaustionInput {
	limits := r.DefaultLimits()
	limits.Timeout = 75 * time.Second
	policy := r.EgressLimits{Requests: 32, InferenceRequests: 24, RefreshRequests: 2, MetadataRequests: 5, NegotiationRequests: 1, RequestBytes: 8 << 20, ResponseBytes: 16 << 20, TotalBytes: 32 << 20}
	return exhaustionInput{Name: name, Err: r.PhaseError{State: "invalid_output"}, Source: &r.SourceAccess{Version: 1}, Policy: policy, Limits: limits,
		Provider: &r.ProviderAccess{Version: 1, Requests: 1, InferenceRequests: 1, Forwarded: 1, Stopped: true, Failed: true},
		Counts:   f.Counts{Inference: 1}, Observation: f.HostileObservation{Results: map[string]string{}, Probe: map[string]string{}}}
}

func TestExhaustionChecksRequireTheMeasuredBound(t *testing.T) {
	flood := exhaustionBase("codex-response-flood")
	flood.Provider.Exhausted, flood.Provider.ResponseBytes = true, 16<<20+1
	flood.Observation.FloodBytes = 17 << 20
	if !allPass(exhaustionChecks(flood)) {
		t.Fatal("measured flood refusal rejected", exhaustionChecks(flood))
	}
	for _, mutate := range []func(*exhaustionInput){
		func(in *exhaustionInput) { in.Provider.Exhausted = false },
		func(in *exhaustionInput) { in.Observation.FloodBytes = 1 << 20 },
		func(in *exhaustionInput) { in.Provider.Responses = 1 },
		func(in *exhaustionInput) { in.Err = errors.New("untyped") },
		func(in *exhaustionInput) { in.Err = nil },
		func(in *exhaustionInput) { in.Report.Findings = []r.Finding{{ID: "x"}} },
		func(in *exhaustionInput) { in.AuditComplete = true },
		func(in *exhaustionInput) { in.Source.ToolCalls = 1 },
	} {
		changed := flood
		provider, source := *flood.Provider, *flood.Source
		changed.Provider, changed.Source = &provider, &source
		mutate(&changed)
		if allPass(exhaustionChecks(changed)) {
			t.Fatal("flood outcome accepted without its bound", changed)
		}
	}

	stall := exhaustionBase("claude-stall")
	stall.Err = errors.Join(r.PhaseError{State: "provider_unavailable"}, context.DeadlineExceeded, r.ErrAuthRecoveryRequired)
	stall.Source = nil
	stall.Observation.Results["stall"] = "cancelled-by-guard"
	stall.Elapsed = 75300 * time.Millisecond
	if !allPass(exhaustionChecks(stall)) {
		t.Fatal("deadline-fenced stall rejected", exhaustionChecks(stall))
	}
	for _, elapsed := range []time.Duration{10 * time.Second, 120 * time.Second} {
		late := stall
		late.Elapsed = elapsed
		if allPass(exhaustionChecks(late)) {
			t.Fatal("stall ended by something other than the phase deadline", elapsed)
		}
	}
	read := stall
	read.StoreRequests = []string{"x:source.go"}
	if allPass(exhaustionChecks(read)) {
		t.Fatal("lost source audit accepted after a store read")
	}
	pending := stall
	pending.Err = errors.Join(r.PhaseError{State: "provider_unavailable"}, r.ErrWorkerStopPending)
	if allPass(exhaustionChecks(pending)) {
		t.Fatal("unproven stop accepted as a typed failure")
	}
}

func TestTurnAndSourceFloodChecks(t *testing.T) {
	codex := exhaustionBase("codex-turn-flood")
	codex.Counts.Inference, codex.Observation.SourceResults = 24, 23
	codex.Provider.InferenceRequests, codex.Provider.Exhausted = 24, true
	codex.Source.ToolCalls = 24
	if !allPass(exhaustionChecks(codex)) {
		t.Fatal("guard-ended turn flood rejected", exhaustionChecks(codex))
	}
	claude := codex
	claudeAccess := *codex.Provider
	claudeAccess.Exhausted, claudeAccess.Failed = false, false
	claude.Name, claude.Provider = "claude-turn-flood", &claudeAccess
	if !allPass(exhaustionChecks(claude)) {
		t.Fatal("client max-turns end rejected", exhaustionChecks(claude))
	}
	short := codex
	short.Counts.Inference = 3
	if allPass(exhaustionChecks(short)) {
		t.Fatal("turn flood ended before the bound accepted")
	}

	source := exhaustionBase("codex-source-flood")
	source.Err, source.AuditComplete = nil, true
	reason := r.Coverage{Reason: "source tool-call limit exhausted"}
	source.Source = &r.SourceAccess{Version: 1, ToolCalls: 128, Exhausted: true, Coverage: []r.Coverage{reason}}
	source.Report.Coverage = []r.Coverage{reason}
	source.Observation.Probe = map[string]string{"ok": "128", "err": "12"}
	source.Provider.Failed = false
	if !allPass(exhaustionChecks(source)) {
		t.Fatal("honest exhausted report rejected", exhaustionChecks(source))
	}
	silent := source
	silent.Report.Coverage = nil
	if allPass(exhaustionChecks(silent)) {
		t.Fatal("complete report without recorded exhaustion accepted")
	}
	lost := source
	lost.Source = nil
	lost.Err = errors.Join(r.PhaseError{State: "invalid_output"}, r.ErrSourceAuditPending, r.ErrAuthRecoveryRequired)
	if allPass(exhaustionChecks(lost)) {
		t.Fatal("lost exhausted audit accepted")
	}
}

func TestOversizeAndCodeHostChecks(t *testing.T) {
	big := exhaustionBase("codex-source-oversize")
	big.Err, big.AuditComplete = nil, true
	omission := r.Coverage{Path: "big.go", Reason: "missing or unreadable object"}
	big.Source = &r.SourceAccess{Version: 1, ToolCalls: 2, ReaderCalls: 2, Bytes: f.BigObjectBytes, Reads: []r.ReadRecord{{Path: "big.go", Bytes: f.BigObjectBytes}}, Coverage: []r.Coverage{omission}}
	big.Report.Coverage = []r.Coverage{omission}
	big.Observation.Probe = map[string]string{"first-read": "ok:1572864:// synthetic", "second-read": "error:21:Source request denied"}
	big.Provider.Failed = false
	if !allPass(exhaustionChecks(big)) {
		t.Fatal("held byte bound rejected", exhaustionChecks(big))
	}
	twice := big
	twice.Source = &r.SourceAccess{Version: 1, Bytes: 2 * f.BigObjectBytes, Reads: []r.ReadRecord{{Path: "big.go"}, {Path: "big.go"}}, Coverage: []r.Coverage{omission}}
	if allPass(exhaustionChecks(twice)) {
		t.Fatal("second big read accepted")
	}
	claude := big
	claude.Name = "claude-source-oversize"
	claude.Observation.Results = map[string]string{"first-read": "other:1259: Error: result exceeds maximum allowed tokens", "second-read": "denied"}
	if !allPass(exhaustionChecks(claude)) {
		t.Fatal("client-truncated first read rejected", exhaustionChecks(claude))
	}
	claude.Observation.Results = map[string]string{"first-read": "full", "second-read": "full"}
	if allPass(exhaustionChecks(claude)) {
		t.Fatal("undenied second read accepted")
	}

	code := exhaustionBase("codex-code-output-flood")
	code.CodeHost, _ = json.Marshal(codeHostAudit{Failed: true, Stopped: true, Stage: "wait"})
	if !allPass(exhaustionChecks(code)) {
		t.Fatal("refused code-host exchange rejected", exhaustionChecks(code))
	}
	code.CodeHost, _ = json.Marshal(codeHostAudit{Stopped: true, RelayComplete: true, Stage: "complete"})
	if allPass(exhaustionChecks(code)) {
		t.Fatal("complete code-host exchange accepted as exhaustion")
	}
	spin := exhaustionBase("codex-code-spin")
	spin.Err, spin.AuditComplete, spin.Elapsed = nil, true, 36*time.Second
	spin.Provider.Failed = false
	spin.Observation.Results["exec-output"] = "61:Script running with cell ID 1 Wall time 31.0 seconds Output: "
	spin.CodeHost, _ = json.Marshal(codeHostAudit{Stopped: true, RelayComplete: true, Stage: "complete"})
	if !allPass(exhaustionChecks(spin)) {
		t.Fatal("yielded spin rejected", exhaustionChecks(spin))
	}
	for _, mutate := range []func(*exhaustionInput){
		func(in *exhaustionInput) { in.Elapsed = 80 * time.Second },
		func(in *exhaustionInput) { in.Observation.Results = map[string]string{"exec-output": "missing"} },
		func(in *exhaustionInput) { in.CodeHost, _ = json.Marshal(codeHostAudit{Stage: "exchange"}) },
	} {
		changed := spin
		mutate(&changed)
		if allPass(exhaustionChecks(changed)) {
			t.Fatal("unbounded spin accepted", changed)
		}
	}
	if checks := exhaustionChecks(exhaustionBase("codex-unknown")); allPass(checks) || checks["known-exhaustion-case"] {
		t.Fatal("unknown case accepted")
	}
	for _, c := range f.ExhaustionCases() {
		if _, ok := exhaustionChecks(exhaustionBase(c.Name))["known-exhaustion-case"]; ok || !strings.HasPrefix(c.Name, string(c.Provider)) {
			t.Fatal("declared case has no outcome checks", c.Name)
		}
	}
}
