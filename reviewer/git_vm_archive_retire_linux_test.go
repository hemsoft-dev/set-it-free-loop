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

func TestGitVMArchiveRetireRemovesBytesAndFreesStaging(t *testing.T) {
	q, wire := gitVMArchiveFixture(t)
	directory := gitVMArchiveTestDir(t)
	a := openGitVMArchiveTest(t, directory)
	// A failed intake stays receiving and blocks all later staging.
	if _, err := a.stage(context.Background(), q, io.NopCloser(bytes.NewReader(wire[:len(wire)/2]))); err == nil {
		t.Fatal("truncated intake staged")
	}
	other := q
	other.Binding.Attempt = strings.Repeat("e", 32)
	if m, err := a.stage(context.Background(), other, io.NopCloser(bytes.NewReader(wire))); err == nil {
		m.Close()
		t.Fatal("receiving intake did not block staging")
	}
	if err := a.retire(context.Background(), q.Binding); err != nil {
		t.Fatal("retire receiving", err)
	}
	if _, err := os.Lstat(filepath.Join(directory, "materialization", gitExportDirectory(q.Binding.Attempt))); !os.IsNotExist(err) {
		t.Fatal("retired bytes remain in the raw archive")
	}
	if m, err := a.reopen(context.Background(), q); err == nil {
		m.Close()
		t.Fatal("retired attempt reopened")
	}
	// Retirement ends the attempt's records, as host retirement does. The
	// factory issues fresh 128-bit random attempts, never a retired one. The
	// next attempt stages normally once the receiving blocker is gone.
	m, err := a.stage(context.Background(), q, io.NopCloser(bytes.NewReader(wire)))
	if err != nil {
		t.Fatal("staging after retirement", err)
	}
	m.Close()
	records, packets, err := a.records(context.Background())
	if err != nil || len(records) != 1 || records[0].State != "staged" || !validAuthHash(packets[q.Binding.Attempt]) {
		t.Fatal("archive records after retirement", records, err)
	}
	if err := a.retire(context.Background(), q.Binding); err != nil {
		t.Fatal("retire staged", err)
	}
	if records, _, err := a.records(context.Background()); err != nil || len(records) != 0 {
		t.Fatal("staged record survived retirement", records, err)
	}
}

func TestGitVMArchiveRetirementResumesAfterInterruption(t *testing.T) {
	for _, point := range []string{"marked", "renamed", "removed"} {
		t.Run(point, func(t *testing.T) {
			q, wire := gitVMArchiveFixture(t)
			directory := gitVMArchiveTestDir(t)
			a, err := openGitVMExportArchive(context.Background(), directory)
			if err != nil {
				t.Fatal(err)
			}
			m, err := a.stage(context.Background(), q, io.NopCloser(bytes.NewReader(wire)))
			if err != nil {
				t.Fatal(err)
			}
			m.Close()
			// Reproduce each durable crash point without completing it.
			j, _ := a.journal()
			j.Records[0].State = "retiring"
			if a.writeJournal(j) != nil {
				t.Fatal("mark retiring")
			}
			if point != "marked" {
				if a.held.root.Mkdir("retired", 0700) != nil || a.held.root.Rename("materialization/"+gitExportDirectory(q.Binding.Attempt), "retired/"+q.Binding.Attempt) != nil {
					t.Fatal("rename")
				}
			}
			if point == "removed" && a.held.root.RemoveAll("retired/"+q.Binding.Attempt) != nil {
				t.Fatal("remove")
			}
			if a.Close(context.Background()) != nil {
				t.Fatal("close interrupted archive")
			}
			reopened := openGitVMArchiveTest(t, directory)
			if m, err := reopened.reopen(context.Background(), q); err == nil {
				m.Close()
				t.Fatal("retiring attempt reopened")
			}
			if err := reopened.resumeRetirements(context.Background()); err != nil {
				t.Fatal("resume", err)
			}
			records, _, err := reopened.records(context.Background())
			if err != nil || len(records) != 0 {
				t.Fatal("retirement not completed", records, err)
			}
			if _, err := os.Lstat(filepath.Join(directory, "retired", q.Binding.Attempt)); !os.IsNotExist(err) {
				t.Fatal("retired residue remains")
			}
		})
	}
}

func TestGitVMArchiveRefusesForeignRetiredResidue(t *testing.T) {
	directory := gitVMArchiveTestDir(t)
	a, err := openGitVMExportArchive(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	if a.Close(context.Background()) != nil {
		t.Fatal("close")
	}
	if os.MkdirAll(filepath.Join(directory, "retired", strings.Repeat("f", 32)), 0700) != nil {
		t.Fatal("foreign residue")
	}
	if a, err := openGitVMExportArchive(context.Background(), directory); err == nil {
		a.Close(context.Background())
		t.Fatal("unowned retired residue accepted")
	}
}
