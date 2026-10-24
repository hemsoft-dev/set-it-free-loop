//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

func TestGitRuntimeDirectoryReconstructsActualProducerArtifact(t *testing.T) {
	build := actualGitRuntime(t)
	root, err := prepareGitRuntime(context.Background(), nativeRuntimePrivateParent(t), bytes.NewReader(build.Artifact), build.Pins, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	fd, err := root.directoryFD()
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	actual, err := verifyGitRuntimeDirectory(context.Background(), fd, build.Pins)
	if err != nil || !reflect.DeepEqual(actual, root.manifest) {
		t.Fatal("actual artifact reconstruction", err)
	}
	for _, path := range []string{"dev/null", "dev/urandom", gitRuntimeMetadata} {
		info, err := root.root.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0444 {
			t.Fatal("fixed placeholder", path, err)
		}
	}
	if root.verify() != nil {
		t.Fatal("verification changed or deleted retained root")
	}
}

func TestGitRuntimeDirectoryRejectsSubstitutionAndExtraStorage(t *testing.T) {
	for _, kind := range []string{"metadata", "extra", "missing-device", "writable", "non-executable", "changed-bytes", "hardlink", "directory-mode", "closed", "cancelled", "wrong-size", "wrong-hash", "legacy-artifact", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			manifest, bodies := gitRuntimeFixture(t)
			raw, pins := gitRuntimeFrame(t, manifest, bodies)
			root, err := prepareGitRuntime(context.Background(), nativeRuntimePrivateParent(t), bytes.NewReader(raw), pins, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			fd, err := root.directoryFD()
			if err != nil {
				t.Fatal(err)
			}
			defer fd.Close()
			ctx := context.Background()
			switch kind {
			case "metadata", "changed-bytes":
				path := gitRuntimeMetadata
				if kind == "changed-bytes" {
					path = gitRuntimeProgram
				}
				root.root.Chmod(path, 0600)
				file, err := root.root.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
				if err != nil {
					t.Fatal(err)
				}
				file.WriteString("substituted release bytes")
				file.Close()
				root.root.Chmod(path, 0444)
			case "extra":
				root.root.Chmod(".", 0700)
				file, err := root.root.OpenFile("extra", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0444)
				if err != nil {
					t.Fatal(err)
				}
				file.Close()
				root.root.Chmod(".", 0555)
			case "missing-device":
				root.root.Chmod("dev", 0700)
				root.root.Remove("dev/null")
				root.root.Chmod("dev", 0555)
			case "writable":
				root.root.Chmod(gitRuntimeProgram, 0755)
			case "non-executable":
				root.root.Chmod(gitRuntimeProgram, 0444)
			case "hardlink":
				parent := filepath.Dir(gitRuntimeProgram)
				root.root.Chmod(parent, 0700)
				if root.root.Link(gitRuntimeProgram, parent+"/link") != nil {
					t.Fatal("link fixture")
				}
				root.root.Chmod(parent, 0555)
			case "directory-mode":
				root.root.Chmod("usr", 0755)
			case "closed":
				fd.Close()
			case "cancelled":
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			case "wrong-size":
				pins.Artifact.Bytes++
			case "wrong-hash":
				pins.Artifact.SHA256 = Digest("wrong artifact")
			case "legacy-artifact":
				fd = gitGrantSealedData(t, raw)
			case "symlink":
				// An in-root link to the exact metadata bytes (issue #257).
				root.root.Chmod(".", 0700)
				if root.root.Rename(gitRuntimeMetadata, "linked-metadata") != nil || root.root.Symlink("linked-metadata", gitRuntimeMetadata) != nil {
					t.Fatal("symlink fixture")
				}
				root.root.Chmod(".", 0555)
			}
			if _, err := verifyGitRuntimeDirectory(ctx, fd, pins); err == nil {
				t.Fatal("substituted directory admitted")
			}
		})
	}
}

func TestGitRuntimeDirectoryRejectsActualNoexecFilesystem(t *testing.T) {
	var fs unix.Statfs_t
	if unix.Statfs("/dev/shm", &fs) != nil || fs.Flags&unix.ST_NOEXEC == 0 {
		t.Skip("no noexec fixture filesystem")
	}
	parent, err := os.MkdirTemp("/dev/shm", "sfl-git-runtime-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(parent)
	m, bodies := gitRuntimeFixture(t)
	raw, pins := gitRuntimeFrame(t, m, bodies)
	root, err := prepareGitRuntime(context.Background(), parent, bytes.NewReader(raw), pins, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	fd, err := root.directoryFD()
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	if _, err := verifyGitRuntimeDirectory(context.Background(), fd, pins); err == nil {
		t.Fatal("runtime on noexec filesystem admitted")
	}
}
