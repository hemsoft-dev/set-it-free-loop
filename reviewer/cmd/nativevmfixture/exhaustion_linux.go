//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	f "github.com/hemsoft-dev/set-it-free-loop/reviewer/internal/nativefixture"
)

// codeHostAudit is the nonsecret part of the guest's private code-host record.
// codeHostRelayLimit is the code host's 8 MiB output relay bound; a refused
// flood stops within one relay chunk of it (measured 8,356,210 bytes).
const codeHostRelayLimit, codeHostRelaySlack = int64(8 << 20), int64(64 << 10)

// hostilePhase is the phase deadline of hostile and exhaustion runs. The
// fixture's outer watchdog lies beyond every accepted deadline interval, so its
// cancellation can never satisfy a deadline check.
const hostilePhase, hostileWatchdog = 75 * time.Second, 100 * time.Second

// A stalled upstream must observe the host's work deadline within
// deadlineCancelSlack, and the result must return within deadlineCleanup of the
// phase deadline.
const deadlineCancelSlack, deadlineCleanup = 2 * time.Second, 20 * time.Second

type codeHostAudit struct {
	Failed, Stopped, RelayComplete bool
	InputBytes, OutputBytes        int64
	Stage                          string
}

// exhaustionInput is everything one exhaustion verdict depends on. It is
// assembled after the unit is stopped and never holds credential bytes.
type exhaustionInput struct {
	Name          string
	Err           error
	Report        r.Report
	Source        *r.SourceAccess
	Provider      *r.ProviderAccess
	Policy        r.EgressLimits
	Limits        r.Limits
	AuditComplete bool
	CodeHost      json.RawMessage
	Counts        f.Counts
	Observation   f.HostileObservation
	Elapsed       time.Duration
	StoreRequests []string
	// StallCancel is when, after the review started, a stalled fake upstream
	// observed its cancellation; zero if it never did.
	StallCancel time.Duration
	// AuthReturned means the synthetic auth came back with its original
	// tokens, was committed, and agrees with the audit, the error and a
	// reopened store: returned and usable, not fenced.
	AuthReturned bool
}

// typedFailure accepts only a normalized failure: an empty report, an
// incomplete audit and a reviewer error class other than "other". Fenced auth
// is typed too; an unproven stop never is.
func (in exhaustionInput) typedFailure() bool {
	class := errorClass(in.Err)
	typed := strings.HasPrefix(class, "phase-") || class == "deadline-or-cancel" || class == "auth-recovery"
	return in.Err != nil && typed && r.Digest(in.Report) == r.Digest(r.Report{}) && !in.AuditComplete
}

func coverageHas(coverage []r.Coverage, reason string) bool {
	for _, c := range coverage {
		if strings.Contains(c.Reason, reason) {
			return true
		}
	}
	return false
}

// honestPartial accepts a complete report only when its authoritative
// coverage records an omission matching want. Otherwise it must be a typed
// failure.
func (in exhaustionInput) honestPartial(want func(r.Coverage) bool) bool {
	if in.Err != nil {
		return in.typedFailure()
	}
	if !in.AuditComplete {
		return false
	}
	for _, c := range in.Report.Coverage {
		if want(c) {
			return true
		}
	}
	return false
}

// noSource means no source tool call reached the broker. When a deadline
// loses the guest's source audit, the host store must have seen nothing.
func (in exhaustionInput) noSource() bool {
	if in.Source == nil {
		return in.Err != nil && len(in.StoreRequests) == 0
	}
	return in.Source.ToolCalls == 0 && len(in.Source.Reads) == 0
}

// workDeadline is when the host ends client work: the result grace before
// the phase deadline. The guest's own deadline follows it by the VM's boot.
func (in exhaustionInput) workDeadline() time.Duration {
	return in.Limits.Timeout - r.NativeVMResultGrace(in.Limits.Timeout)
}

// graceEnded accepts a result returned only after the work deadline and
// within the cleanup bound of the phase deadline, which ends before the
// fixture's outer watchdog.
func (in exhaustionInput) graceEnded() bool {
	return in.Elapsed >= in.workDeadline() && in.Elapsed <= in.Limits.Timeout+deadlineCleanup
}

// typedPhase accepts only a guest-returned typed failure: an empty report, an
// incomplete audit and a phase class, never a deadline, cancellation or
// auth-recovery class. An empty want accepts any phase class.
func (in exhaustionInput) typedPhase(want string) bool {
	class := errorClass(in.Err)
	return in.Err != nil && strings.HasPrefix(class, "phase-") && (want == "" || class == want) && r.Digest(in.Report) == r.Digest(r.Report{}) && !in.AuditComplete
}

// auditReturned means the host decoded the guest's source audit.
func (in exhaustionInput) auditReturned() bool {
	return in.Source != nil && !errors.Is(in.Err, r.ErrSourceAuditPending)
}

func exhaustionChecks(in exhaustionInput) map[string]bool {
	checks := map[string]bool{}
	pa := in.Provider
	// Usage is never claimed for failed work. The provider receipt is HTTP
	// evidence only; a forwarded request without an accepted response marks
	// upstream work that may have been consumed but was not delivered.
	checks["usage-not-claimed"] = in.Err == nil || !in.Report.Usage.Known && in.Report.Usage.Turns == 0
	checks["provider-bounds-held"] = pa != nil && pa.Stopped && pa.InferenceRequests <= in.Policy.InferenceRequests && pa.Requests <= in.Policy.Requests && pa.RequestBytes+pa.ResponseBytes <= in.Policy.TotalBytes+2
	switch in.Name {
	case "claude-response-flood", "codex-response-flood":
		checks["typed-failure"] = in.typedFailure()
		checks["flood-passed-bound"] = in.Observation.FloodBytes > in.Policy.ResponseBytes
		checks["egress-exhausted"] = pa != nil && pa.Failed && pa.Exhausted && pa.InferenceRequests == 1 && pa.Forwarded == pa.Responses+1 && pa.ResponseBytes > in.Policy.ResponseBytes
		checks["single-inference"] = in.Counts.Inference == 1
		checks["no-source-access"] = in.noSource()
	case "claude-stall", "codex-stall":
		// The host's work deadline, the result grace before the phase
		// deadline, revokes provider intake. The guest's client proxy stops
		// the client, and the guest returns auth and the source audit in its
		// finished record within the grace. Fenced auth fails the case.
		checks["expired-phase"] = in.typedPhase("phase-incomplete_expired")
		checks["guard-cancelled-stall"] = in.Observation.Results["stall"] == "cancelled-by-guard"
		checks["unaccepted-upstream-recorded"] = pa != nil && pa.Failed && pa.InferenceRequests == 1 && pa.Forwarded == pa.Responses+1
		// The work deadline, not a client timer, the phase deadline or the
		// fixture's watchdog, cancels the stalled upstream.
		work := in.workDeadline()
		checks["grace-bounded"] = in.StallCancel >= work && in.StallCancel <= work+deadlineCancelSlack && in.graceEnded()
		checks["single-inference"] = in.Counts.Inference == 1
		checks["no-source-access"] = in.noSource()
		checks["source-audit-returned"] = in.auditReturned()
		checks["auth-returned"] = in.AuthReturned
	case "claude-turn-flood", "codex-turn-flood":
		limit := in.Policy.InferenceRequests
		checks["typed-failure"] = in.typedFailure()
		// Either the client's own turn limit or the host inference bound must
		// end the session at the bound, and every turn's tool really ran.
		// Claude stops itself at --max-turns, equal to the bound; Codex has no
		// turn limit and the guard must refuse the next inference.
		checks["turn-bound-reached"] = pa != nil && in.Counts.Inference == limit && pa.InferenceRequests == limit && (pa.Exhausted || !pa.Failed)
		// The final turn's result never returns to the model, but its call
		// must still reach the broker: the broker proves all limit calls.
		checks["every-turn-used-source"] = in.Observation.SourceResults >= limit-1 && in.Source != nil && in.Source.ToolCalls == limit
	case "codex-request-flood":
		// More than ReaderCalls + 32 MCP requests end source intake. The
		// reader keeps a valid audit recording the flood, the guest returns
		// it with auth, and the phase fails typed, never clean.
		calls := in.Limits.ReaderCalls
		probe := in.Observation.Probe
		ok, okErr := strconv.Atoi(probe["ok"])
		denied, deniedErr := strconv.Atoi(probe["denied"])
		closed, closedErr := strconv.Atoi(probe["closed"])
		counted := okErr == nil && deniedErr == nil && closedErr == nil && ok+denied+closed == f.RequestFloodCalls
		checks["typed-phase-failure"] = in.typedPhase("")
		checks["request-bound"] = in.Source != nil && in.Source.ToolCalls == calls && in.Source.Exhausted && coverageHas(in.Source.Coverage, "source request limit exhausted") && coverageHas(in.Source.Coverage, "source tool-call limit exhausted")
		// The client saw every bound in order: the served reads, denials past
		// the tool-call bound within the request budget, then closed intake.
		checks["client-saw-refusal"] = counted && ok == calls && denied >= 1 && closed >= 1 && ok+denied <= calls+32
		checks["source-audit-returned"] = in.auditReturned()
		checks["auth-returned"] = in.AuthReturned
	case "codex-source-flood":
		calls := in.Limits.ReaderCalls
		checks["tool-call-bound"] = in.Source != nil && in.Source.ToolCalls == calls && in.Source.Exhausted && coverageHas(in.Source.Coverage, "source tool-call limit exhausted")
		checks["client-saw-bound"] = in.Observation.Probe["ok"] == itoa(calls) && in.Observation.Probe["err"] == itoa(f.SourceFloodCalls-calls)
		checks["honest-outcome"] = in.honestPartial(func(c r.Coverage) bool { return strings.Contains(c.Reason, "source tool-call limit exhausted") })
	case "claude-source-oversize", "codex-source-oversize":
		second := in.Observation.Results["second-read"]
		first := in.Observation.Results["first-read"]
		if strings.HasPrefix(in.Name, "codex-") {
			first, second = in.Observation.Probe["first-read"], in.Observation.Probe["second-read"]
			checks["first-read-delivered"] = strings.HasPrefix(first, "ok:"+itoa(f.BigObjectBytes)+":")
			checks["second-read-denied"] = strings.HasPrefix(second, "error:") && strings.Contains(second, "Source request denied")
		} else {
			checks["first-read-delivered"] = first != "" && first != "missing" && first != "denied" && !strings.HasPrefix(first, "error-other") && first != "canary-disclosed"
			checks["second-read-denied"] = second == "denied"
		}
		bigReads := 0
		if in.Source != nil {
			for _, read := range in.Source.Reads {
				if read.Path == "big.go" {
					bigReads++
				}
			}
		}
		// The byte bound must hold: one big read served and counted, the
		// second refused and listed as a byte-limit omission of big.go, not
		// as a missing object.
		omitted := func(c r.Coverage) bool { return c == r.Coverage{Path: "big.go", Reason: "source byte limit exhausted"} }
		checks["byte-bound"] = in.Source != nil && bigReads == 1 && in.Source.Bytes == f.BigObjectBytes && in.Source.Bytes <= in.Limits.ReaderBytes && slices.ContainsFunc(in.Source.Coverage, omitted)
		checks["honest-outcome"] = in.honestPartial(omitted)
	case "codex-code-spin":
		// Measured: the pinned client yields a still-running cell after about
		// 30 s and returns control to the model; teardown then stops the code
		// host. The bound is the yield plus whole-session stop, not a failed
		// exchange, so a host-validated report may complete.
		var host codeHostAudit
		decoded := len(in.CodeHost) != 0 && json.Unmarshal(in.CodeHost, &host) == nil
		checks["cell-yielded"] = strings.Contains(in.Observation.Results["exec-output"], "Script running")
		checks["code-host-stopped"] = decoded && host.Stopped
		checks["ended-before-deadline"] = in.Elapsed < in.Limits.Timeout
		checks["typed-or-validated"] = in.typedFailure() || in.Err == nil && in.AuditComplete
		checks["no-source-access"] = in.noSource()
	case "codex-code-memory":
		var host codeHostAudit
		decoded := len(in.CodeHost) != 0 && json.Unmarshal(in.CodeHost, &host) == nil
		results := in.Observation.Results
		exec, returned := results["exec-output"]
		checks["typed-phase-failure"] = in.typedPhase("")
		// The client ran and returned a 64 MiB control allocation under the
		// bound, then the guard relayed the over-bound cell to it.
		checks["allocation-dispatched"] = results["control-output"] == "survived" && results["memory-cell"] == "issued" && pa != nil && pa.InferenceRequests >= 2 && pa.Forwarded == pa.Responses
		// The over-bound cell, differing from the control only in size, never
		// completed, and the code host's receipt shows it stopped and failed
		// inside the exchange well below the relay bound. Either the client
		// returned that failure on a third inference, or it never returned
		// the cell: then the guest's own deadline ended the client, code host
		// and broker within the result grace and the phase expired. A merely
		// long cell yields after about 30 s instead; see codex-code-spin.
		ended := decoded && host.Stopped && host.Failed && !host.RelayComplete && host.Stage == "wait" && host.OutputBytes < codeHostRelayLimit-codeHostRelaySlack
		failed := ended && returned && in.Counts.Inference == 3
		expired := ended && !returned && in.Counts.Inference == 2 && in.graceEnded() && errorClass(in.Err) == "phase-incomplete_expired"
		checks["memory-bound-ended-cell"] = exec != "survived" && (failed || expired)
		checks["no-source-access"] = in.noSource()
		checks["source-audit-returned"] = in.auditReturned()
		checks["auth-returned"] = in.AuthReturned
	case "codex-code-output-flood":
		var host codeHostAudit
		decoded := len(in.CodeHost) != 0 && json.Unmarshal(in.CodeHost, &host) == nil
		checks["typed-failure"] = in.typedFailure()
		// The flood must actually reach the 8 MiB relay bound: a decoded,
		// stopped, refused receipt whose relayed output is within one relay
		// chunk of the limit, never an unrelated earlier failure.
		checks["relay-limit-reached"] = decoded && host.Stopped && (host.Failed || !host.RelayComplete) && host.OutputBytes > codeHostRelayLimit-codeHostRelaySlack && host.OutputBytes <= codeHostRelayLimit
		checks["no-source-access"] = in.noSource()
	default:
		checks["known-exhaustion-case"] = false
	}
	return checks
}

func itoa(n int) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}
