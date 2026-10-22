//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
)

type stopProbe struct {
	stopErr, cleanupErr, closeErr       error
	stopCalls, cleanupCalls, closeCalls int
}

func (p *stopProbe) ConfirmAuthStopped(context.Context, r.AuthBinding) error {
	p.stopCalls++
	return p.stopErr
}
func (p *stopProbe) CleanupRuntime(context.Context, r.AuthBinding) error {
	p.cleanupCalls++
	return p.cleanupErr
}
func (p *stopProbe) Close() error { p.closeCalls++; return p.closeErr }
func TestOriginalStopPropagatesCloseAndEarlierFailures(t *testing.T) {
	raw, err := os.ReadFile("testdata/actual-native-unit.json")
	if err != nil {
		t.Fatal(err)
	}
	var unit unitRecord
	if err = json.Unmarshal(raw, &unit); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dir := filepath.Join(root, "launches", unit.Binding.LaunchID)
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "unit.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	stopErr, cleanupErr, closeErr := errors.New("stop"), errors.New("cleanup"), errors.New("close")
	for _, p := range []*stopProbe{{}, {closeErr: closeErr}, {stopErr: stopErr, closeErr: closeErr}, {cleanupErr: cleanupErr, closeErr: closeErr}} {
		_, err := stopOriginalWith(config{Root: root}, unit.Binding.SessionHash, func(context.Context, string, string, string) (stoppedSupervisor, error) { return p, nil })
		if p.closeCalls != 1 || p.stopCalls != 1 {
			t.Fatal("owner not closed exactly once", p)
		}
		for _, expected := range []error{p.stopErr, p.cleanupErr, p.closeErr} {
			if expected != nil && !errors.Is(err, expected) {
				t.Fatal("lost lifecycle error", expected, err)
			}
		}
		if p.stopErr == nil && p.cleanupErr == nil && p.closeErr == nil && err != nil {
			t.Fatal(err)
		}
	}
}
func TestRetainedAuthJournalUsesActualExportedProducer(t *testing.T) {
	raw, err := os.ReadFile("testdata/actual-exported-auth-journal.json")
	if err != nil {
		t.Fatal(err)
	}
	var original authRecoveryJournal
	if err = json.Unmarshal(raw, &original); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dir := filepath.Join(root, "auth", string(original.Provider))
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	lease := filepath.Join(dir, "lease.json")
	if err = os.WriteFile(lease, raw, 0600); err != nil {
		t.Fatal(err)
	}
	c := config{Root: root, Provider: original.Provider}
	digest, err := retainRecoveryJournal(c, original.Binding, original.SeedHash)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(lease); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(filepath.Join(root, "retained-auth-recovery.json"))
	if err != nil {
		t.Fatal(err)
	}
	var after authRecoveryJournal
	if err = json.Unmarshal(saved, &after); err != nil {
		t.Fatal(err)
	}
	if r.Digest(original) != r.Digest(after) || hash(saved) != digest {
		t.Fatal("pre-retirement evidence changed")
	}
}
func TestAuthSnapshotRefusesChangedProvenanceAndPreservesOriginal(t *testing.T) {
	raw, err := os.ReadFile("testdata/actual-exported-auth-journal.json")
	if err != nil {
		t.Fatal(err)
	}
	var original authRecoveryJournal
	if err = json.Unmarshal(raw, &original); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*authRecoveryJournal){func(j *authRecoveryJournal) { j.Provider = r.Claude }, func(j *authRecoveryJournal) { j.Stage = "committed" }, func(j *authRecoveryJournal) { j.TransferStage = "returned" }, func(j *authRecoveryJournal) { j.Binding.SessionHash = "changed" }} {
		root := t.TempDir()
		dir := filepath.Join(root, "auth", string(original.Provider))
		os.MkdirAll(dir, 0700)
		changed := original
		mutate(&changed)
		body, _ := json.Marshal(changed)
		lease := filepath.Join(dir, "lease.json")
		os.WriteFile(lease, body, 0600)
		if _, err = retainRecoveryJournal(config{Root: root, Provider: original.Provider}, original.Binding, original.SeedHash); err == nil {
			t.Fatal("changed journal accepted")
		}
		if _, err = os.Stat(lease); err != nil {
			t.Fatal("original recovery evidence removed")
		}
		if _, err = os.Stat(filepath.Join(root, "retained-auth-recovery.json")); !os.IsNotExist(err) {
			t.Fatal("invalid snapshot written")
		}
	}
}
