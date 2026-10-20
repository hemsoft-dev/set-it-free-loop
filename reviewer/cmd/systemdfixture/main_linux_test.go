//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDeferredCleanupInvalidatesTentativePass(t *testing.T) {
	for _, prior := range []string{"", "original exercise failure"} {
		receipt := result{Passed: prior == "", Error: prior}
		fallback, closed := 0, 0
		finish(&receipt, func(context.Context) error { return errors.New("stop failed") }, func(context.Context) error { fallback++; return nil }, func() error { closed++; return nil })
		if receipt.Passed || !receipt.FallbackCleanup || receipt.Error == "" || fallback != 1 || closed != 1 {
			t.Fatal("cleanup manufactured success", receipt, fallback, closed)
		}
		if prior != "" && receipt.Error != prior {
			t.Fatal("cleanup replaced primary failure", receipt.Error)
		}
	}
}
func TestDeferredCloseFailureInvalidatesPass(t *testing.T) {
	receipt := result{Passed: true}
	finish(&receipt, func(context.Context) error { return nil }, func(context.Context) error { t.Fatal("unneeded fallback"); return nil }, func() error { return errors.New("close failed") })
	if receipt.Passed || receipt.FallbackCleanup || receipt.Error == "" {
		t.Fatal(receipt)
	}
}
func TestDeferredCleanStopPreservesPass(t *testing.T) {
	receipt := result{Passed: true}
	finish(&receipt, func(context.Context) error { return nil }, func(context.Context) error { t.Fatal("unneeded fallback"); return nil }, func() error { return nil })
	if !receipt.Passed || receipt.FallbackCleanup || receipt.Error != "" {
		t.Fatal(receipt)
	}
}

func TestFallbackCleanupGetsFreshDeadlineAfterStop(t *testing.T) {
	receipt := result{Passed: true}
	var stopContext context.Context
	finish(&receipt, func(ctx context.Context) error { stopContext = ctx; return context.DeadlineExceeded }, func(ctx context.Context) error {
		if stopContext.Err() == nil || ctx.Err() != nil {
			t.Fatal("fallback reused stop context")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("fallback unbounded")
		}
		return exec.CommandContext(ctx, "/usr/bin/true").Run()
	}, func() error { return nil })
	if receipt.Passed || !receipt.FallbackCleanup {
		t.Fatal(receipt)
	}
}
func TestRecoveredCloseFailureCannotQualify(t *testing.T) {
	receipt := result{Passed: true}
	checkedCleanup(&receipt, "recovered supervisor close failed", func() error { return errors.New("transport close") })
	if receipt.Passed || receipt.Error != "recovered supervisor close failed" {
		t.Fatal(receipt)
	}
}
func TestFinalJournalRequiredForPassingReceipt(t *testing.T) {
	binding := r.AuthBinding{SessionHash: r.Digest("session"), LaunchID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ContainmentID: r.Digest("containment")}
	for _, kind := range []string{"missing", "invalid", "wrong-binding", "not-stopped", "valid"} {
		t.Run(kind, func(t *testing.T) {
			receipt := result{Passed: true, Binding: binding, Unit: "fixture.service", StubSHA256: r.Digest("stub")}
			path := filepath.Join(t.TempDir(), "unit.json")
			evidence := map[string]any{"Binding": binding, "Unit": receipt.Unit, "StubHash": receipt.StubSHA256, "Captured": true, "Aborted": true, "Stopped": true}
			if kind == "wrong-binding" {
				evidence["Unit"] = "other.service"
			}
			if kind == "not-stopped" {
				evidence["Stopped"] = false
			}
			raw, _ := json.Marshal(evidence)
			if kind == "invalid" {
				raw = []byte("invalid")
			}
			if kind != "missing" {
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			retainJournal(&receipt, path)
			if (kind == "valid") != receipt.Passed {
				t.Fatal(kind, receipt)
			}
			if kind == "valid" && string(receipt.Journal) != string(raw) {
				t.Fatal("journal bytes changed")
			}
		})
	}
}
