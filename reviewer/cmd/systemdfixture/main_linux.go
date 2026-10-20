//go:build linux

// systemdfixture explicitly exercises a transient fake-only unit and synthetic
// auth store. Normal tests never call it or the host's systemd manager.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
)

type result struct {
	Version                                             int           `json:"version"`
	Passed                                              bool          `json:"passed"`
	FakeOnly                                            bool          `json:"fake_only"`
	ProductionQualified                                 bool          `json:"production_qualified"`
	Binding                                             r.AuthBinding `json:"binding"`
	Unit                                                string        `json:"unit"`
	StubSHA256                                          string        `json:"stub_sha256"`
	Started, Granted, Stopped, AuthCompleted, Recovered bool
	EscapedChildPID                                     int             `json:"escaped_child_pid"`
	OriginalPathReplaced                                bool            `json:"original_path_replaced"`
	EscapedChildStopped                                 bool            `json:"escaped_child_stopped"`
	Journal                                             json.RawMessage `json:"private_unit_journal,omitempty"`
	Error                                               string          `json:"error,omitempty"`
	FallbackCleanup                                     bool            `json:"fallback_cleanup"`
}

func (receipt *result) cleanupFailed(stage string) {
	receipt.Passed = false
	if receipt.Error == "" {
		receipt.Error = stage
	}
}

// Keep finalization testable without starting a host service. Cleanup is part
// of qualification, including after the exercise has tentatively passed.
func boundedCleanup(action func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return action(ctx)
}
func checkedCleanup(receipt *result, stage string, action func() error) {
	if action() != nil {
		receipt.cleanupFailed(stage)
	}
}
func retainJournal(receipt *result, path string) {
	raw, err := os.ReadFile(path)
	var evidence struct {
		Binding                    r.AuthBinding
		Unit, StubHash             string
		Captured, Aborted, Stopped bool
	}
	if err != nil || len(raw) > 65536 || !json.Valid(raw) || json.Unmarshal(raw, &evidence) != nil || evidence.Binding != receipt.Binding || evidence.Unit != receipt.Unit || evidence.StubHash != receipt.StubSHA256 {
		receipt.cleanupFailed("final durable journal unavailable or invalid")
		return
	}
	receipt.Journal = raw
	if receipt.Passed && (!evidence.Captured || !evidence.Aborted || !evidence.Stopped) {
		receipt.cleanupFailed("final durable stop evidence incomplete")
	}
}
func finish(receipt *result, stop, fallback func(context.Context) error, closeSupervisor func() error) {
	if err := boundedCleanup(stop); err != nil {
		receipt.FallbackCleanup = true
		receipt.cleanupFailed("deferred supervisor stop failed")
		_ = boundedCleanup(fallback)
	}
	checkedCleanup(receipt, "deferred supervisor close failed", closeSupervisor)
}

func exercise(root, stub, hash string) (receipt result) {
	receipt = result{Version: 1, FakeOnly: true, StubSHA256: hash}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		receipt.Error = "invalid fresh fixture root"
		return
	}
	if err := os.Mkdir(root, 0700); err != nil {
		receipt.Error = "fixture root must be new"
		return
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		receipt.Error = "nonce unavailable"
		return
	}
	manifest := r.FakeGateManifest{Version: 1, Marker: "synthetic host systemd marker"}
	receipt.Binding = r.AuthBinding{SessionHash: r.Digest(manifest), LaunchID: hex.EncodeToString(nonce[:]), ContainmentID: r.Digest("fake-only containment " + hex.EncodeToString(nonce[:]))}
	receipt.Unit = "sfl-alpha-" + receipt.Binding.LaunchID + ".service"
	gates := filepath.Join(root, "gates")
	authRoot := filepath.Join(root, "synthetic-auth")
	if os.Mkdir(gates, 0700) != nil || os.Mkdir(authRoot, 0700) != nil {
		receipt.Error = "fixture directories unavailable"
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rawStub, err := os.ReadFile(stub)
	if err != nil {
		receipt.Error = "fixture image unavailable"
		return
	}
	mutableStub := filepath.Join(root, "mutable-unitstub")
	if os.WriteFile(mutableStub, rawStub, 0500) != nil {
		receipt.Error = "fixture image copy unavailable"
		return
	}
	supervisor, err := r.OpenSystemdSupervisor(ctx, gates, mutableStub, hash)
	if err != nil {
		receipt.Error = "open supervisor: " + err.Error()
		return
	}
	defer func() {
		finish(&receipt,
			func(cleanup context.Context) error { return supervisor.AbortAndWait(cleanup, receipt.Binding) },
			func(fallback context.Context) error {
				return exec.CommandContext(fallback, "/usr/bin/systemctl", "--user", "stop", receipt.Unit).Run()
			}, supervisor.Close)
		retainJournal(&receipt, filepath.Join(gates, receipt.Binding.LaunchID, "unit.json"))
	}()
	// Race regression: replace the supplied pathname after verification. Both
	// the main process and its self-exec child must still use the sealed image.
	replacement := filepath.Join(root, "unpinned-replacement")
	if os.WriteFile(replacement, []byte("unpinned replacement must never execute"), 0500) != nil || os.Rename(replacement, mutableStub) != nil {
		receipt.Error = "path replacement unavailable"
		return
	}
	receipt.OriginalPathReplaced = true
	if err = supervisor.PrepareFake(receipt.Binding, manifest); err != nil {
		receipt.Error = "prepare: " + err.Error()
		return
	}
	auth, err := r.NewAuthStore(r.Codex, authRoot)
	if err != nil {
		receipt.Error = "synthetic auth store: " + err.Error()
		return
	}
	if err = auth.Refresh(ctx, func([]byte) ([]byte, error) { return []byte("synthetic offline unit fixture state"), nil }); err != nil {
		receipt.Error = "synthetic seed: " + err.Error()
		return
	}
	profile, err := auth.LeaseProfile(ctx, receipt.Binding)
	if err != nil {
		receipt.Error = "synthetic lease: " + err.Error()
		return
	}
	defer checkedCleanup(&receipt, "synthetic profile close failed", profile.Close)
	if err = supervisor.StartFake(ctx, receipt.Binding); err != nil {
		receipt.Error = "start: " + err.Error()
		return
	}
	receipt.Started = true
	if err = supervisor.GrantFake(ctx, receipt.Binding, manifest); err != nil {
		receipt.Error = "grant: " + err.Error()
		return
	}
	receipt.Granted = true
	var child struct{ Version, PID, Session int }
	deadline := time.Now().Add(2 * time.Second)
	for {
		raw, readErr := os.ReadFile(filepath.Join(gates, receipt.Binding.LaunchID, "fake-child.json"))
		if readErr == nil && json.Unmarshal(raw, &child) == nil {
			break
		}
		if time.Now().After(deadline) {
			receipt.Error = "escaped fake child was not observed"
			return
		}
		time.Sleep(time.Millisecond)
	}
	if child.Version != 1 || child.PID <= 0 || child.Session != child.PID {
		receipt.Error = "invalid escaped-child identity"
		return
	}
	childFD, err := unix.PidfdOpen(child.PID, 0)
	if err != nil {
		receipt.Error = "escaped child already absent"
		return
	}
	defer checkedCleanup(&receipt, "child pidfd close failed", func() error { return unix.Close(childFD) })
	if unix.PidfdSendSignal(childFD, 0, nil, 0) != nil {
		receipt.Error = "escaped child not live before stop"
		return
	}
	receipt.EscapedChildPID = child.PID
	if err = supervisor.AbortAndWait(ctx, receipt.Binding); err != nil {
		receipt.Error = "stop: " + err.Error()
		return
	}
	receipt.Stopped = true
	ready := []unix.PollFd{{Fd: int32(childFD), Events: unix.POLLIN}}
	if count, pollErr := unix.Poll(ready, 1000); pollErr != nil || count != 1 || ready[0].Revents&unix.POLLIN == 0 {
		receipt.Error = "escaped child survived whole-unit stop"
		return
	}
	receipt.EscapedChildStopped = true
	if err = supervisor.Close(); err != nil {
		receipt.Error = "close stopped transport: " + err.Error()
		return
	}
	recovered, err := r.OpenSystemdSupervisor(ctx, gates, mutableStub, hash)
	if err != nil {
		receipt.Error = "reopen: " + err.Error()
		return
	}
	recoveredClosed := false
	defer func() {
		if !recoveredClosed {
			checkedCleanup(&receipt, "recovered supervisor close failed", recovered.Close)
		}
	}()
	if err = recovered.ConfirmAuthStopped(ctx, receipt.Binding); err != nil {
		receipt.Error = "recovered stop: " + err.Error()
		return
	}
	receipt.Recovered = true
	if err = profile.Complete(ctx, recovered); err != nil {
		receipt.Error = "synthetic lease completion: " + err.Error()
		return
	}
	receipt.AuthCompleted = true
	recoveredClosed = true
	if err = recovered.Close(); err != nil {
		receipt.Error = "recovered supervisor close failed"
		return
	}
	receipt.Passed = true
	return
}
func main() {
	root := flag.String("root", "", "new private fixture directory")
	stub := flag.String("stub", "", "pinned fixed unitstub binary")
	hash := flag.String("stub-sha256", "", "expected stub digest")
	flag.Parse()
	receipt := exercise(*root, *stub, *hash)
	raw, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		os.Exit(2)
	}
	if receipt.Binding.LaunchID != "" {
		if os.WriteFile(filepath.Join(*root, "result.json"), append(raw, '\n'), 0600) != nil {
			receipt.cleanupFailed("fixture receipt could not be retained")
			raw, _ = json.MarshalIndent(receipt, "", "  ")
		}
	}
	if _, err := fmt.Println(string(raw)); err != nil {
		os.Exit(2)
	}
	if !receipt.Passed {
		os.Exit(1)
	}
}
