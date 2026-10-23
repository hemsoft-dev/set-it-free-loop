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
	limits.Timeout = hostilePhase
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
	// Measured: the upstream was cancelled at the 75 s deadline and the result
	// returned 0.2 to 0.3 s later.
	stall.Elapsed, stall.StallCancel = 75300*time.Millisecond, 75050*time.Millisecond
	if !allPass(exhaustionChecks(stall)) {
		t.Fatal("deadline-fenced stall rejected", exhaustionChecks(stall))
	}
	for _, ended := range [][2]time.Duration{
		{10 * time.Second, 9 * time.Second},
		{120 * time.Second, 75050 * time.Millisecond},
		// The fixture's outer watchdog, not the phase deadline, cancelled it.
		{hostileWatchdog + 300*time.Millisecond, hostileWatchdog},
		// No upstream cancellation was observed at all.
		{75300 * time.Millisecond, 0},
		{75300 * time.Millisecond, 70 * time.Second},
	} {
		late := stall
		late.Elapsed, late.StallCancel = ended[0], ended[1]
		if allPass(exhaustionChecks(late)) {
			t.Fatal("stall ended by something other than the phase deadline", ended)
		}
	}
	if hostilePhase+deadlineCleanup >= hostileWatchdog {
		t.Fatal("accepted deadline interval reaches the outer watchdog")
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
	// The final turn's source call must reach the broker, even though its
	// result never returns to the model.
	lastLost := codex
	lastLostSource := *codex.Source
	lastLostSource.ToolCalls = 23
	lastLost.Source = &lastLostSource
	if allPass(exhaustionChecks(lastLost)) {
		t.Fatal("turn flood accepted without the final turn's broker call")
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
	// Measured receipt: the relay stopped within one chunk of 8 MiB.
	code.CodeHost, _ = json.Marshal(codeHostAudit{Failed: true, Stopped: true, Stage: "wait", OutputBytes: 8356210})
	if !allPass(exhaustionChecks(code)) {
		t.Fatal("refused code-host exchange rejected", exhaustionChecks(code))
	}
	// An unrelated earlier failure never exercised the relay bound.
	for _, unrelated := range []any{nil, codeHostAudit{Failed: true, Stopped: true, Stage: "start"}, codeHostAudit{Failed: true, Stopped: true, OutputBytes: 9 << 20}} {
		changed := code
		changed.CodeHost = nil
		if unrelated != nil {
			changed.CodeHost, _ = json.Marshal(unrelated)
		}
		if allPass(exhaustionChecks(changed)) {
			t.Fatal("output flood accepted without reaching the relay limit", unrelated)
		}
	}
	memoryChecks(t)
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

// The memory case passes only when the client ran an allocation under the
// bound, the guard relayed the over-bound cell, and that cell never completed:
// the code host failed inside the exchange, or the client never returned it
// and only the phase deadline ended the session.
func memoryChecks(t *testing.T) {
	t.Helper()
	// Measured shape: the client never returned the over-bound cell and the
	// deadline fenced auth with the code-host receipt lost.
	hung := exhaustionBase("codex-code-memory")
	hung.Err = errors.Join(r.PhaseError{State: "provider_unavailable"}, context.DeadlineExceeded, r.ErrAuthRecoveryRequired)
	hung.Source, hung.CodeHost, hung.Elapsed = nil, nil, 75300*time.Millisecond
	hung.Counts.Inference = 2
	hung.Provider = &r.ProviderAccess{Version: 1, Requests: 8, InferenceRequests: 2, Forwarded: 7, Responses: 7, Stopped: true, Failed: true}
	hung.Observation.Results = map[string]string{"control-output": "survived", "memory-cell": "issued"}
	if !allPass(exhaustionChecks(hung)) {
		t.Fatal("deadline-ended over-bound cell rejected", exhaustionChecks(hung))
	}
	killed := hung
	killed.Err, killed.Elapsed = r.PhaseError{State: "invalid_output"}, 6*time.Second
	killed.Counts.Inference = 3
	killed.Provider = &r.ProviderAccess{Version: 1, Requests: 9, InferenceRequests: 3, Forwarded: 8, Responses: 8, Stopped: true, Failed: true}
	killed.Observation.Results = map[string]string{"control-output": "survived", "memory-cell": "issued", "exec-output": "60:failed to read code-mode host message: early eof"}
	killed.CodeHost, _ = json.Marshal(codeHostAudit{Failed: true, Stopped: true, Stage: "wait", OutputBytes: 120})
	if !allPass(exhaustionChecks(killed)) {
		t.Fatal("code host failed by the over-bound cell rejected", exhaustionChecks(killed))
	}
	// A client or code host that fails before running the allocation, even
	// immediately and with no memory pressure, never passes.
	early := exhaustionBase("codex-code-memory")
	early.CodeHost, early.Elapsed = nil, 5*time.Second
	for name, in := range map[string]exhaustionInput{"early-typed-failure": early, "early-failure-at-deadline": func() exhaustionInput { in := early; in.Elapsed = hung.Elapsed; return in }()} {
		if allPass(exhaustionChecks(in)) {
			t.Fatal("memory case passed without dispatching the allocation", name)
		}
	}
	accepted, _ := json.Marshal(codeHostAudit{Stopped: true, RelayComplete: true, Stage: "complete"})
	for name, mutate := range map[string]func(*exhaustionInput){
		"no-control-output":   func(in *exhaustionInput) { delete(in.Observation.Results, "control-output") },
		"control-failed":      func(in *exhaustionInput) { in.Observation.Results["control-output"] = "9:early eof" },
		"cell-not-issued":     func(in *exhaustionInput) { delete(in.Observation.Results, "memory-cell") },
		"cell-refused":        func(in *exhaustionInput) { in.Provider.Responses-- },
		"cell-survived":       func(in *exhaustionInput) { in.Observation.Results["exec-output"] = "survived" },
		"returned-before-end": func(in *exhaustionInput) { in.Elapsed = 5 * time.Second },
		"watchdog-ended":      func(in *exhaustionInput) { in.Elapsed = hostileWatchdog + 300*time.Millisecond },
		"accepted-receipt":    func(in *exhaustionInput) { in.CodeHost = accepted },
	} {
		changed := hung
		provider := *hung.Provider
		changed.Provider, changed.Observation.Results = &provider, map[string]string{}
		for k, v := range hung.Observation.Results {
			changed.Observation.Results[k] = v
		}
		mutate(&changed)
		if allPass(exhaustionChecks(changed)) {
			t.Fatal("deadline-ended memory case accepted without its evidence", name)
		}
	}
	for name, host := range map[string]any{
		"before-exchange": codeHostAudit{Failed: true, Stopped: true, Stage: "probe"},
		"relay-bound":     codeHostAudit{Failed: true, Stopped: true, Stage: "wait", OutputBytes: 8356210},
		"not-stopped":     codeHostAudit{Failed: true, Stage: "wait"},
		"completed":       codeHostAudit{Stopped: true, RelayComplete: true, Stage: "complete"},
		"lost":            nil,
	} {
		changed := killed
		changed.CodeHost = nil
		if host != nil {
			changed.CodeHost, _ = json.Marshal(host)
		}
		if allPass(exhaustionChecks(changed)) {
			t.Fatal("code-host failure not attributable to the over-bound cell", name)
		}
	}
}
