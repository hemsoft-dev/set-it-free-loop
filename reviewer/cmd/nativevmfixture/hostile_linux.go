//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	f "github.com/hemsoft-dev/set-it-free-loop/reviewer/internal/nativefixture"
)

type hostileAudit struct {
	Complete, AuthExported, AuthReturned bool
	Binding                              r.AuthBinding
	Stage, GuestStage, Failure           string
	CodeHost                             json.RawMessage
}

type hostileCanaryHits struct{ Requests, Report, Files, Receipt int }

type hostileReceipt struct {
	Version                                                      int
	Purpose, RunnerSourceHead, RunnerSourceTree, ImageSourceHead string
	Case, Kind                                                   string
	Provider                                                     r.Provider
	Passed, HostContained, FakeOnly                              bool
	RuntimeQualified, ProviderEligibilityQualified               bool
	Checks                                                       map[string]bool
	State, ErrorClass, ErrorText, Stage, GuestStage, Failure     string
	ElapsedMS                                                    int64
	Counts                                                       f.Counts
	Observation                                                  f.HostileObservation
	StoreRequests                                                []string
	DeniedDestinations                                           int64
	CanaryHits                                                   hostileCanaryHits
	Source                                                       *r.SourceAccess
	ProviderAccess                                               *r.ProviderAccess
	CodeHost                                                     json.RawMessage
}

func errorClass(err error) string {
	var phase r.PhaseError
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, r.ErrWorkerStopPending):
		return "stop-pending"
	case errors.Is(err, r.ErrAuthRecoveryRequired):
		return "auth-recovery"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "deadline-or-cancel"
	case errors.As(err, &phase):
		return "phase-" + phase.State
	}
	return "other"
}

// errorText keeps a bounded diagnostic of static reviewer error strings. The
// whole receipt is canary-scanned before it is written.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	text := strings.ReplaceAll(err.Error(), "\n", "; ")
	if len(text) > 512 {
		text = text[:512]
	}
	return text
}

// ProbeContained accepts code-mode facts only when no module, process,
// filesystem, network or extra tool capability was observed.
func probeContained(probe map[string]string) bool {
	if len(probe) == 0 || probe["tools"] != "mcp__sfl_source__read,mcp__sfl_source__search" || probe["all_tools"] != probe["tools"] || probe["env"] != "absent" {
		return false
	}
	for _, name := range []string{"require", "process", "Deno", "Bun", "XMLHttpRequest", "WebSocket", "Worker", "importScripts"} {
		if probe["typeof:"+name] != "undefined" {
			return false
		}
	}
	imports, fetches, toolsSeen, media, loads := 0, 0, 0, 0, 0
	for key, value := range probe {
		switch {
		case strings.HasPrefix(key, "image:"), strings.HasPrefix(key, "audio:"):
			media++
			if !strings.HasPrefix(value, "throw:") {
				return false
			}
		case strings.HasPrefix(key, "load:"):
			loads++
			if value != "returned:undefined" {
				return false
			}
		case strings.HasPrefix(key, "import:"):
			imports++
			if !strings.HasPrefix(value, "throw:") {
				return false
			}
		case strings.HasPrefix(key, "fetch:"):
			fetches++
			if value != "absent" && !strings.HasPrefix(value, "throw:") {
				return false
			}
		case strings.HasPrefix(key, "tool:"):
			toolsSeen++
			if value != "undefined" {
				return false
			}
		}
	}
	return imports == 7 && fetches == 3 && toolsSeen == 8 && media == 4 && loads == 2
}

// Without model source calls, only the host's post-stop anchor check may read.
func hostAnchorOnly(requests []string, head string) bool {
	for _, request := range requests {
		if request != head+":source.go" {
			return false
		}
	}
	return len(requests) <= 1
}

func fileCanaryHits(root string, secrets []string) int {
	hits := 0
	paths, _ := filepath.Glob(filepath.Join(root, "launches", "*", "*.json"))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil || len(raw) > 1<<20 {
			hits++
			continue
		}
		hits += f.CanaryHits(raw, secrets)
	}
	return hits
}

func runNativeHostile(c config) error {
	hc, ok := f.LookupHostile(c.Hostile, c.Provider)
	if !ok {
		return errors.New("unknown hostile case")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	canaries, err := f.NewCanaries()
	if err != nil {
		return err
	}
	for _, dir := range []string{"auth", "launches"} {
		if os.Mkdir(filepath.Join(c.Root, dir), 0700) != nil {
			return errors.New("fresh hostile directories required")
		}
	}
	// The host canary never enters a guest channel. A later disclosure would
	// mean host state crossed into the VM, provider request or receipt.
	hostCanary := filepath.Join(c.Root, "host-canary")
	if os.WriteFile(hostCanary, []byte(canaries.Host), 0600) != nil || os.Chmod(hostCanary, 0) != nil {
		return errors.New("host canary failed")
	}
	store, err := r.NewAuthStore(c.Provider, filepath.Join(c.Root, "auth"))
	if err != nil {
		return err
	}
	seed := f.Seed(c.Provider)
	err = store.Refresh(ctx, func(old []byte) ([]byte, error) {
		if len(old) != 0 {
			return nil, errors.New("existing synthetic seed denied")
		}
		return seed, nil
	})
	clear(seed)
	if err != nil {
		return err
	}
	limits := r.DefaultLimits()
	limits.Timeout = 75 * time.Second
	snapshot := r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: 1, Head: strings.Repeat("a", 40), Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("c", 40)}
	s, err := r.NewSession("native-fake-hostile-"+hc.Name, snapshot, r.Digest("synthetic-native-hostile"), c.Provider, 1, "immutable source evidence", nil, nil, limits)
	if err != nil {
		return err
	}
	objects := f.NewHostileStore(canaries)
	reader, err := r.NewReader(objects, snapshot, f.HostilePaths(), limits)
	if err != nil {
		return err
	}
	backend, policy, err := f.NewHostile(s, hc.Name, canaries)
	if err != nil {
		return err
	}
	transport, closeUpstream := protocolUpstream(backend)
	defer closeUpstream()
	var denied atomic.Int64
	transport.denied = &denied
	hints := r.NativeVMProfileHints{}
	if c.Provider == r.Claude {
		hints = r.NativeVMProfileHints{ClaudeAccountUUID: "11111111-1111-4111-8111-111111111111", ClaudeOrganizationUUID: "22222222-2222-4222-8222-222222222222", ClaudeEmail: "fixture@example.invalid"}
	}
	worker, err := r.NewNativeVMReviewer(r.NativeVMOptions{Provider: c.Provider, Root: filepath.Join(c.Root, "launches"), Stub: c.Stub.Path, StubHash: c.Stub.SHA256, QEMU: c.QEMU, Kernel: c.Kernel, Initrd: c.Initrd, Firmware: c.Firmware, Runtime: c.Runtime, Auth: store, Reader: reader, Policy: policy, Transport: transport, ProfileHints: hints, Check: func(_ context.Context, got r.Session) error {
		if r.Digest(got) != r.Digest(s) {
			return errors.New("foreign hostile session")
		}
		return nil
	}})
	if err != nil {
		return err
	}
	start := time.Now()
	report, access, evidence, reviewErr := worker.ReviewAudited(ctx, s)
	elapsed := time.Since(start).Milliseconds()
	counts, obs := backend.Counts(), backend.Hostile()
	secrets := canaries.Secrets(c.Provider)
	returned := false
	authErr := store.Refresh(context.WithoutCancel(ctx), func(current []byte) ([]byte, error) { returned = len(current) > 0; return current, nil })
	paths, globErr := filepath.Glob(filepath.Join(c.Root, "launches", "*", "unit.json"))
	var unit cancellationUnitReceipt
	var audit hostileAudit
	unitRead := globErr == nil && len(paths) == 1 && readProtocolJSON(paths[0], &unit) == nil && readProtocolJSON(filepath.Join(filepath.Dir(paths[0]), "native-audit.json"), &audit) == nil
	bound := unitRead && unit.Binding.SessionHash == r.Digest(s) && unit.Binding.LaunchID == filepath.Base(filepath.Dir(paths[0])) && audit.Binding == unit.Binding
	roots, rootErr := filepath.Glob(filepath.Join(c.Root, "launches", "*", "qemu-runtime"))
	reportRaw, _ := json.Marshal(report)
	requests := objects.Requests()
	stopped := evidence != nil && evidence.Access.Stopped
	checks := map[string]bool{
		"unit-captured-and-stopped": bound && unit.Captured && unit.Stopped,
		"auth-returned":             authErr == nil && returned && audit.AuthExported && audit.AuthReturned && !errors.Is(reviewErr, r.ErrAuthRecoveryRequired),
		"runtime-root-removed":      rootErr == nil && len(roots) == 0,
		"provider-stopped":          stopped,
		"no-stop-pending":           !errors.Is(reviewErr, r.ErrWorkerStopPending),
		"no-canary-in-requests":     backend.CanaryRequestHits() == 0,
		"no-canary-in-report":       f.CanaryHits(reportRaw, secrets) == 0,
		"no-canary-in-launch-files": fileCanaryHits(c.Root, secrets) == 0,
		"no-denied-destination":     denied.Load() == 0,
		"no-telemetry":              counts.Telemetry == 0 && counts.Negotiation == 0,
		"no-backend-problem":        len(obs.Problems) == 0,
	}
	if c.Provider == r.Claude {
		checks["inventory-fixed"] = slices.Equal(obs.Inventory, []string{"StructuredOutput", "mcp__sfl-source__read", "mcp__sfl-source__search"})
	} else {
		checks["inventory-fixed"] = f.CodexInventoryAllowed(obs.Inventory)
	}
	switch hc.Kind {
	case f.HostileGuard:
		checks["typed-guard-refusal"] = reviewErr != nil && r.Digest(report) == r.Digest(r.Report{}) && evidence != nil && evidence.Access.Failed && !audit.Complete
		checks["single-inference"] = counts.Inference == 1 && obs.Turns == 1 && counts.Failures == 0
		checks["no-source-access"] = access != nil && access.ToolCalls == 0 && len(access.Reads) == 0 && len(requests) == 0
	case f.HostileSource:
		checks["complete-report"] = reviewErr == nil && len(report.Findings) == 1 && evidence != nil && !evidence.Access.Failed && audit.Complete
		turns := 2
		if c.Provider == r.Claude {
			turns = f.HostileSourceCalls() + 1
		}
		checks["planned-inferences"] = counts.Inference == turns && counts.Failures == 0
		checks["hostile-results-denied"] = f.HostileSourcePassed(obs.Results)
		reads := []string{}
		if access != nil {
			for _, read := range access.Reads {
				reads = append(reads, read.Revision+":"+read.Path)
			}
		}
		slices.Sort(reads)
		checks["every-call-reached-broker"] = access != nil && access.ToolCalls == f.HostileSourceCalls()
		checks["only-allowed-reads"] = slices.Equal(reads, []string{s.Snapshot.Head + ":inject.md", s.Snapshot.Head + ":source.go"})
		// The host verifies report anchors through the same store after stop.
		sorted := slices.Clone(requests)
		slices.Sort(sorted)
		sorted = slices.Compact(sorted)
		checks["store-saw-only-allowlisted"] = slices.Equal(sorted, []string{s.Snapshot.Head + ":inject.md", s.Snapshot.Head + ":link.go", s.Snapshot.Head + ":source.go"})
	case f.HostileProbe:
		checks["complete-report"] = reviewErr == nil && len(report.Findings) == 1 && evidence != nil && !evidence.Access.Failed && audit.Complete
		checks["planned-inferences"] = counts.Inference == 2 && counts.Failures == 0
		checks["code-host-contained"] = probeContained(obs.Probe) && obs.MediaInputs == 0
		checks["no-source-access"] = access != nil && access.ToolCalls == 0 && hostAnchorOnly(requests, s.Snapshot.Head)
	case f.HostileMetadata:
		if checks["inventory-fixed"] {
			checks["complete-report"] = reviewErr == nil && len(report.Findings) == 1 && evidence != nil && !evidence.Access.Failed && audit.Complete
			checks["planned-inferences"] = counts.Inference == 2 && counts.Failures == 0
			checks["only-allowed-reads"] = access != nil && access.ToolCalls == 1 && len(access.Reads) == 1 && access.Reads[0].Path == "source.go" && len(requests) == 2
		} else {
			checks["typed-guard-refusal"] = reviewErr != nil && r.Digest(report) == r.Digest(r.Report{}) && evidence != nil && evidence.Access.Failed && !audit.Complete
			checks["single-inference"] = counts.Inference == 1 && counts.Failures == 0
			checks["no-source-access"] = access != nil && access.ToolCalls == 0 && hostAnchorOnly(requests, s.Snapshot.Head)
		}
	}
	// HostContained excludes the client's own advertised inventory. A changed
	// inventory remains a failed case even when every host layer held.
	passed, contained := true, true
	for name, ok := range checks {
		passed = passed && ok
		if name != "inventory-fixed" && name != "no-backend-problem" {
			contained = contained && ok
		}
	}
	state := "complete"
	if reviewErr != nil {
		state = "failed"
	}
	receipt := hostileReceipt{Version: 1, Purpose: "native-fake-hostile", RunnerSourceHead: sourceHead, RunnerSourceTree: sourceTree, ImageSourceHead: c.ImageSourceHead, Case: hc.Name, Kind: hc.Kind, Provider: c.Provider, FakeOnly: true, Checks: checks, State: state, ErrorClass: errorClass(reviewErr), ErrorText: errorText(reviewErr), Stage: audit.Stage, GuestStage: audit.GuestStage, Failure: audit.Failure, ElapsedMS: elapsed, Counts: counts, Observation: obs, StoreRequests: requests, DeniedDestinations: denied.Load(), Source: access, CodeHost: audit.CodeHost}
	if evidence != nil {
		receipt.ProviderAccess = &evidence.Access
	}
	receipt.CanaryHits = hostileCanaryHits{backend.CanaryRequestHits(), f.CanaryHits(reportRaw, secrets), fileCanaryHits(c.Root, secrets), 0}
	receipt.Passed, receipt.HostContained = passed, contained
	data, _ := json.MarshalIndent(receipt, "", "  ")
	if hits := f.CanaryHits(data, secrets); hits != 0 {
		receipt.CanaryHits.Receipt, receipt.Passed, receipt.HostContained, receipt.Observation, receipt.CodeHost = hits, false, false, f.HostileObservation{Case: hc.Name, Kind: hc.Kind}, nil
		data, _ = json.MarshalIndent(receipt, "", "  ")
	}
	if os.WriteFile(filepath.Join(c.Root, "result.json"), append(data, '\n'), 0600) != nil {
		return errors.New("hostile receipt failed")
	}
	fmt.Println(string(data))
	if !receipt.Passed {
		return errors.New("native hostile qualification incomplete")
	}
	return nil
}
