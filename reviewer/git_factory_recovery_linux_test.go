//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitFactoryRecoveryDiscardsIncompleteIntakeOnlyAfterStop(t *testing.T) {
	f := newGitUnitFixture(t)
	f.prepareStart(t)
	a, dir := exportArchiveFixture(t)
	if m, err := a.stage(context.Background(), f.binding, io.NopCloser(strings.NewReader("partial"))); err == nil || m != nil {
		t.Fatal("partial export unexpectedly staged")
	}
	f.manager.holdPopulated = true
	if a.discardStoppedIntake(context.Background(), f.owner, f.binding) == nil {
		t.Fatal("populated group allowed discard")
	}
	attempt, err := a.attempt(f.binding.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	record, err := readGitExportRecord(attempt)
	attempt.Close()
	if err != nil || record.State != "receiving" {
		t.Fatal("failed stop changed intake receipt", record, err)
	}
	f.manager.holdPopulated = false
	if a.discardStoppedIntake(context.Background(), f.owner, f.binding) != nil {
		t.Fatal("known stopped intake could not be discarded")
	}
	if _, err := os.Stat(filepath.Join(dir, gitExportDirectory(f.binding.Attempt), "payload.bin")); !os.IsNotExist(err) {
		t.Fatal("discarded payload remains", err)
	}
	if m, err := a.reopen(context.Background(), f.binding); err == nil || m != nil {
		t.Fatal("discarded intake became source")
	}
	if a.discardStoppedIntake(context.Background(), f.owner, f.binding) != nil {
		t.Fatal("discard was not restart-idempotent")
	}
	if m, err := a.stage(context.Background(), f.binding, io.NopCloser(bytes.NewReader(nil))); err == nil || m != nil {
		t.Fatal("spent nonce was recaptured")
	}
	_, next, _ := gitMaterializationFixture(t, "sha1")
	next.Attempt = strings.Repeat("c", 32)
	// A different nonce can reach intake again after recovery. Deliberately bad
	// bytes create a distinct receiving record, proving it passed admission.
	_, _ = a.stage(context.Background(), next, io.NopCloser(strings.NewReader("bad")))
	if _, err := os.Stat(filepath.Join(dir, gitExportDirectory(next.Attempt), "archive.json")); err != nil {
		t.Fatal("terminal discarded receipt blocked all later nonces", err)
	}
}

func TestGitFactoryRecoveryRefusesStagedWireWithoutWorkerGrant(t *testing.T) {
	wire, binding, _ := gitMaterializationFixture(t, "sha1")
	f := newGitUnitFixture(t)
	f.binding = binding
	if f.owner.prepare(context.Background(), binding) != nil {
		t.Fatal("prepare")
	}
	a, _ := exportArchiveFixture(t)
	m, err := a.stage(context.Background(), binding, io.NopCloser(bytes.NewReader(wire)))
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
	if retained, err := recoverGitFactory(context.Background(), f.owner, a); err == nil || retained != nil {
		t.Fatal("archive alone manufactured producer admission")
	}
}

func TestGitFactoryRecoveryRefusesOrphanOrMismatchedArchive(t *testing.T) {
	for _, mode := range []string{"orphan", "binding"} {
		t.Run(mode, func(t *testing.T) {
			wire, binding, _ := gitMaterializationFixture(t, "sha1")
			f := newGitUnitFixture(t)
			a, _ := exportArchiveFixture(t)
			if mode == "binding" {
				f.binding = binding
				f.binding.ReleaseHash = Digest("different release")
				if f.owner.prepare(context.Background(), f.binding) != nil {
					t.Fatal("prepare")
				}
			}
			m, err := a.stage(context.Background(), binding, io.NopCloser(bytes.NewReader(wire)))
			if err != nil {
				t.Fatal(err)
			}
			m.Close()
			if retained, err := recoverGitFactory(context.Background(), f.owner, a); err == nil || retained != nil {
				t.Fatal("orphan/mismatched source recovered")
			}
		})
	}
}

func TestGitFactoryArchiveClosedDescriptorIsTerminal(t *testing.T) {
	a, _ := exportArchiveFixture(t)
	if a.lock.Close() != nil {
		t.Fatal("close actual lock descriptor")
	}
	if a.Close(context.Background()) == nil || !a.closed {
		t.Fatal("failed close retained fictional ownership")
	}
	if a.Close(context.Background()) != nil {
		t.Fatal("terminal close was not idempotent")
	}
}
