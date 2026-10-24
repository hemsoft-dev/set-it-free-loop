//go:build linux

package reviewer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These are algorithm tests using small trusted synthetic objects. Production
// must run this plumbing, capture and export inside the measured bounded unit.
func gitQuarantineFixture(t *testing.T, formats ...string) (GitStore, Snapshot) {
	t.Helper()
	format := "sha1"
	if len(formats) == 1 {
		format = formats[0]
	}
	f := snapshotGitFixtureFormat(t, format)
	return gitQuarantineFromFixture(t, f, format)
}
func gitQuarantineFromFixture(t *testing.T, f snapshotGitFixture, format string) (GitStore, Snapshot) {
	t.Helper()
	f.run(t, "update-ref", "refs/heads/base", f.snapshot.Base)
	directory := filepath.Join(t.TempDir(), "objects.git")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		command := exec.Command(f.git, args...)
		command.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
		if raw, err := command.CombinedOutput(); err != nil {
			t.Fatalf("synthetic Git %v: %s %v", args, raw, err)
		}
	}
	run("init", "--bare", "--object-format="+format, "--template=", directory)
	if err := os.WriteFile(filepath.Join(directory, "config"), []byte(gitAcquisitionConfig(f.snapshot)), 0600); err != nil {
		t.Fatal(err)
	}
	run("--git-dir="+directory, "-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "fetch.unpackLimit=1", "fetch", "--no-auto-maintenance", "--no-write-fetch-head", "--no-tags", f.dir, "HEAD:refs/sfl/head", "refs/heads/base:refs/sfl/base")
	if err := os.WriteFile(filepath.Join(directory, "HEAD"), []byte(f.snapshot.Head+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return GitStore{f.git, directory}, f.snapshot
}
func TestGitQuarantineActualGitCompleteGraphAndExactMetadata(t *testing.T) {
	store, snapshot := gitQuarantineFixture(t)
	inventory, err := verifyGitQuarantine(context.Background(), store, snapshot, DefaultGitAcquisitionBounds())
	if err != nil {
		filepath.WalkDir(store.GitDir, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr == nil {
				relative, _ := filepath.Rel(store.GitDir, path)
				t.Logf("synthetic path %s", relative)
			}
			return walkErr
		})
		command := exec.Command(store.Git, "--git-dir="+store.GitDir, "fsck", "--full", "--strict", "--no-reflogs", "--no-dangling")
		command.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "LC_ALL=C"}
		raw, commandErr := command.CombinedOutput()
		t.Logf("synthetic fsck %q %v", raw, commandErr)
		t.Fatal(err)
	}
	if inventory.Version != 1 || inventory.SnapshotHash != Digest(snapshot) || !validAuthHash(inventory.ObjectsHash) || inventory.Objects != 14 || inventory.Commits != 3 || inventory.ExpandedBytes <= 0 || inventory.DiskBytes <= 0 {
		t.Fatal("actual synthetic inventory differs", inventory)
	}
	// Existing packet capture is exercised here only against trusted synthetic
	// input. Production capture stays inside the acquisition unit as well.
	evidence, err := CaptureSnapshotEvidence(context.Background(), store, snapshot)
	if err != nil || evidence == nil {
		t.Fatal("verified synthetic snapshot capture", err)
	}
}
func TestGitQuarantineRejectsMetadataAndObjectStoreEscapeRoutes(t *testing.T) {
	for _, kind := range []string{"shallow", "alternates", "graft", "promisor", "commit-graph", "replace", "config", "head", "symlink", "hardlink", "corrupt-pack"} {
		t.Run(kind, func(t *testing.T) {
			store, snapshot := gitQuarantineFixture(t)
			write := func(name, content string) {
				t.Helper()
				path := filepath.Join(store.GitDir, name)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "shallow":
				write("shallow", snapshot.MergeBase+"\n")
			case "alternates":
				write("objects/info/alternates", "/synthetic-outside-object-store\n")
			case "graft":
				write("info/grafts", snapshot.Head+"\n")
			case "promisor":
				write("objects/pack/pack-"+snapshot.Head+".promisor", "")
			case "commit-graph":
				write("objects/info/commit-graph", "synthetic graph cache")
			case "replace":
				write("refs/replace/"+snapshot.Head, snapshot.Base+"\n")
			case "config":
				write("config", gitAcquisitionConfig(snapshot)+"[fsck]\n\tmissingEmail = ignore\n")
			case "head":
				write("refs/sfl/head", snapshot.Base+"\n")
			case "symlink":
				if err := os.Symlink("/synthetic-outside", filepath.Join(store.GitDir, "objects", "info", "alternates")); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(filepath.Join(store.GitDir, "config"), filepath.Join(t.TempDir(), "linked-config")); err != nil {
					t.Fatal(err)
				}
			case "corrupt-pack":
				entries, err := os.ReadDir(filepath.Join(store.GitDir, "objects", "pack"))
				if err != nil {
					t.Fatal(err)
				}
				changed := false
				for _, entry := range entries {
					if strings.HasSuffix(entry.Name(), ".pack") {
						packPath := filepath.Join(store.GitDir, "objects", "pack", entry.Name())
						if err := os.Chmod(packPath, 0600); err != nil {
							t.Fatal(err)
						}
						file, err := os.OpenFile(packPath, os.O_WRONLY, 0)
						if err != nil {
							t.Fatal(err)
						}
						if _, err := file.WriteAt([]byte("BAD!"), 0); err != nil {
							t.Fatal(err)
						}
						file.Close()
						changed = true
						break
					}
				}
				if !changed {
					t.Fatal("synthetic fetch produced no pack")
				}
			}
			if result, err := verifyGitQuarantine(context.Background(), store, snapshot, DefaultGitAcquisitionBounds()); err == nil || result != (GitAcquisitionInventory{}) {
				t.Fatal("invalid quarantine yielded inventory", result, err)
			}
		})
	}
}

// A definite object, commit or byte overrun of a lowered bound is the
// permanent history_limit refusal. The filesystem entry bound and invalid
// bounds stay opaque. No case yields a partial inventory.
func TestGitQuarantineBoundsAndCanceledInputRefusePartialInventory(t *testing.T) {
	store, snapshot := gitQuarantineFixture(t)
	for _, kind := range []string{"objects", "commits", "files", "object-bytes", "expanded", "disk", "invalid"} {
		t.Run(kind, func(t *testing.T) {
			bounds := DefaultGitAcquisitionBounds()
			switch kind {
			case "objects":
				bounds.Objects = 1
			case "commits":
				bounds.Commits = 1
			case "files":
				bounds.Files = 1
			case "object-bytes":
				bounds.ObjectBytes = 1
			case "expanded":
				bounds.ExpandedBytes = 1
			case "disk":
				bounds.DiskBytes = 1
			case "invalid":
				bounds.Objects++
			}
			result, err := verifyGitQuarantine(context.Background(), store, snapshot, bounds)
			if err == nil || result != (GitAcquisitionInventory{}) {
				t.Fatal("excess quarantine accepted", kind, result, err)
			}
			opaque := kind == "files" || kind == "invalid"
			if opaque && !errors.Is(err, ErrGitQuarantine) || !opaque && err != (CaptureRefusal{"history_limit"}) {
				t.Fatal("bound overrun misclassified", kind, err)
			}
			cancelled, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := verifyGitQuarantine(cancelled, store, snapshot, bounds); !errors.Is(err, ErrGitQuarantine) {
				t.Fatal("cancelled overrun reported", kind, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, ctx := range []context.Context{nil, ctx} {
		if result, err := verifyGitQuarantine(ctx, store, snapshot, DefaultGitAcquisitionBounds()); err == nil || result != (GitAcquisitionInventory{}) {
			t.Fatal("invalid context yielded inventory")
		}
	}
	wrong := snapshot
	wrong.MergeBase = snapshot.Base
	if _, err := verifyGitQuarantine(context.Background(), store, wrong, DefaultGitAcquisitionBounds()); err == nil {
		t.Fatal("incorrect merge base accepted")
	}
}

func TestGitQuarantineActualSHA256Objects(t *testing.T) {
	store, snapshot := gitQuarantineFixture(t, "sha256")
	inventory, err := verifyGitQuarantine(context.Background(), store, snapshot, DefaultGitAcquisitionBounds())
	if err != nil || inventory.Objects != 14 || len(snapshot.Head) != 64 {
		t.Fatal("SHA256 quarantine rejected", inventory, err)
	}
}
func TestGitQuarantineMissingParentCannotYieldQualifiedInventory(t *testing.T) {
	store, snapshot := gitQuarantineFixture(t)
	tree, err := store.command(context.Background(), 128, "rev-parse", snapshot.Head+"^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	writeCommit := func(parent string, at int) string {
		t.Helper()
		raw := fmt.Sprintf("tree %s\nparent %s\nauthor Synthetic <fixture@example.invalid> %d +0000\ncommitter Synthetic <fixture@example.invalid> %d +0000\n\nsynthetic incomplete parent graph\n", strings.TrimSpace(string(tree)), parent, at, at)
		command := exec.Command(store.Git, "--git-dir="+store.GitDir, "hash-object", "-w", "-t", "commit", "--stdin")
		command.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "LC_ALL=C"}
		command.Stdin = strings.NewReader(raw)
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatal(err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	missing := strings.Repeat("1", 40)
	snapshot.MergeBase = writeCommit(missing, 1700000000)
	snapshot.Base = writeCommit(snapshot.MergeBase, 1700000100)
	snapshot.Head = writeCommit(snapshot.MergeBase, 1700000200)
	for name, text := range map[string]string{"HEAD": snapshot.Head + "\n", "refs/sfl/head": snapshot.Head + "\n", "refs/sfl/base": snapshot.Base + "\n"} {
		if err := os.WriteFile(filepath.Join(store.GitDir, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{snapshot.Head, snapshot.Base, snapshot.MergeBase} {
		if kind, err := store.command(context.Background(), 32, "cat-file", "-t", id); err != nil || string(kind) != "commit\n" {
			t.Fatal("expected tip/merge-base commit absent", err)
		}
	}
	if inventory, err := verifyGitQuarantine(context.Background(), store, snapshot, DefaultGitAcquisitionBounds()); err == nil || inventory != (GitAcquisitionInventory{}) {
		t.Fatal("present tips and merge-base hid missing parent", inventory, err)
	}
}
