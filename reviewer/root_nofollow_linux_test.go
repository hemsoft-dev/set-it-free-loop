//go:build linux

package reviewer

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// Every audited *os.Root + O_NOFOLLOW call site refuses an in-root symlink
// placed exactly where it opens (issue #257).
func TestRootNoFollowCallSitesRefuseInRootSymlinks(t *testing.T) {
	ctx := context.Background()
	linked := func(t *testing.T, link, content string) (string, *os.Root) {
		t.Helper()
		directory, root := rootNoFollowFixture(t)
		authMustWrite(t, filepath.Join(directory, "real.json"), content)
		rootNoFollowLink(t, "real.json", filepath.Join(directory, link))
		return directory, root
	}
	t.Run("git VM owner journal", func(t *testing.T) {
		_, root := linked(t, "source-vm-units.json", "{}")
		if _, err := readGitVMOwnerFile(root, false); err == nil {
			t.Fatal("owner journal followed an in-root symlink")
		}
	})
	t.Run("cgroup kill", func(t *testing.T) {
		directory, root := linked(t, "cgroup.kill", "")
		if err := killOpenedCgroup(ctx, root, cgroupBinding{}); err == nil {
			t.Fatal("cgroup.kill followed an in-root symlink")
		}
		if b, err := os.ReadFile(filepath.Join(directory, "real.json")); err != nil || len(b) != 0 {
			t.Fatal("kill written through a link", string(b), err)
		}
	})
	t.Run("cgroup file", func(t *testing.T) {
		_, root := linked(t, "cgroup.events", "populated 0")
		if _, err := cgroupFile(root, "cgroup.events"); err == nil {
			t.Fatal("cgroup file followed an in-root symlink")
		}
	})
	t.Run("quarantine file", func(t *testing.T) {
		_, root := linked(t, "HEAD", "ref: refs/heads/main\n")
		if gitQuarantineFileEquals(root, "HEAD", "ref: refs/heads/main\n") {
			t.Fatal("quarantine file followed an in-root symlink")
		}
	})
	t.Run("native evidence", func(t *testing.T) {
		_, root := linked(t, "native-request.json", "{}")
		if _, err := readNativeEvidenceFile(root, "native-request.json", 4096); err == nil {
			t.Fatal("native evidence followed an in-root symlink")
		}
	})
	t.Run("git VM archive lock", func(t *testing.T) {
		_, root := linked(t, "git-export.lock", "")
		if f, err := lockGitVMArchiveRoot(ctx, root, "git-export.lock"); err == nil {
			f.Close()
			t.Fatal("archive lock followed an in-root symlink")
		}
	})
	t.Run("git retirement directory", func(t *testing.T) {
		_, root := linked(t, "archive.json", "{}")
		if _, _, err := gitRetirementDirectory(root, map[string]bool{"archive.json": true}); err == nil {
			t.Fatal("retirement directory accepted an in-root symlink")
		}
	})
	t.Run("reviewer trigger policy", func(t *testing.T) {
		directory := nativeRuntimePrivateParent(t)
		if err := gitReviewerPersistPolicy(ctx, directory, alphaEnrollment(), TriggerPolicy{}); err != nil {
			t.Fatal(err)
		}
		const name = "reviewer-trigger-policy.json"
		if err := os.Rename(filepath.Join(directory, name), filepath.Join(directory, "real.json")); err != nil {
			t.Fatal(err)
		}
		rootNoFollowLink(t, "real.json", filepath.Join(directory, name))
		if err := gitReviewerPersistPolicy(ctx, directory, alphaEnrollment(), TriggerPolicy{}); err == nil {
			t.Fatal("trigger policy followed an in-root symlink")
		}
	})
	for _, link := range []string{"promotion.json", "promotion.lock"} {
		t.Run("release promotion "+link, func(t *testing.T) {
			directory := nativeRuntimePrivateParent(t)
			registry, err := CreateReleasePromotionRegistry(ctx, directory, alphaEnrollment())
			if err != nil {
				t.Fatal(err)
			}
			// startup.lock is an allowed registry entry, so only the link differs.
			if err := os.Rename(filepath.Join(directory, link), filepath.Join(directory, "startup.lock")); err != nil {
				t.Fatal(err)
			}
			rootNoFollowLink(t, "startup.lock", filepath.Join(directory, link))
			if _, err := registry.Snapshot(ctx); !errors.Is(err, ErrFenced) {
				t.Fatal("promotion registry followed an in-root symlink", err)
			}
		})
	}
	t.Run("git export archive lock", func(t *testing.T) {
		directory := gitVMArchiveTestDir(t)
		rootNoFollowLink(t, "planted.lock", filepath.Join(directory, "git-export.lock"))
		if a, err := openGitExportArchive(ctx, directory); err == nil {
			a.Close(ctx)
			t.Fatal("export lock followed a dangling in-root symlink")
		}
		if _, err := os.Lstat(filepath.Join(directory, "planted.lock")); !os.IsNotExist(err) {
			t.Fatal("export lock created through a link", err)
		}
	})
	t.Run("git unit owner lock", func(t *testing.T) {
		directory := nativeRuntimePrivateParent(t)
		rootNoFollowLink(t, "planted.lock", filepath.Join(directory, "git-owner.lock"))
		kernel := &fakeContainmentKernel{}
		manager := &fakeContainmentManager{owner: "synthetic-bus/:1.77", kernel: kernel}
		if owner, err := newGitUnitOwner(ctx, directory, "/nonexistent-stub", Digest("stub"), manager, kernel); err == nil {
			owner.Close()
			t.Fatal("owner lock followed a dangling in-root symlink")
		}
		if _, err := os.Lstat(filepath.Join(directory, "planted.lock")); !os.IsNotExist(err) {
			t.Fatal("owner lock created through a link", err)
		}
	})
	for _, kind := range []string{"final", "intermediate"} {
		t.Run("native guest profile "+kind, func(t *testing.T) {
			directory := t.TempDir()
			other := filepath.Join(directory, "profile", string(Claude))
			if err := os.MkdirAll(other, 0700); err != nil {
				t.Fatal(err)
			}
			authMustWrite(t, filepath.Join(other, "auth.json"), "other-provider-auth")
			authMustWrite(t, filepath.Join(other, ".credentials.json"), "other-provider-auth")
			own := filepath.Join(directory, "profile", string(Codex))
			if kind == "final" {
				rootNoFollowLink(t, "../"+string(Claude)+"/.credentials.json", filepath.Join(own, "auth.json"))
			} else {
				rootNoFollowLink(t, string(Claude), own)
			}
			if _, err := nativeGuestReadProfileFile(directory, Codex, os.Geteuid()); !errors.Is(err, ErrAuthTransfer) {
				t.Fatal("guest profile followed an in-root symlink", err)
			}
		})
	}
	t.Run("native runtime pin", func(t *testing.T) {
		parent := nativeRuntimePrivateParent(t)
		bin := filepath.Join(parent, "rt", "bin")
		if err := os.MkdirAll(bin, 0700); err != nil {
			t.Fatal(err)
		}
		body := "runtime-bytes"
		if err := os.WriteFile(filepath.Join(bin, "real"), []byte(body), 0444); err != nil {
			t.Fatal(err)
		}
		rootNoFollowLink(t, "real", filepath.Join(bin, "tool"))
		if err := os.Chmod(bin, 0555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(bin, 0700) })
		parentRoot, err := os.OpenRoot(parent)
		if err != nil {
			t.Fatal(err)
		}
		defer parentRoot.Close()
		root, err := parentRoot.OpenRoot("rt")
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		identity, err := parentRoot.Lstat("rt")
		if err != nil {
			t.Fatal(err)
		}
		pin := func(path string) nativeRuntimeFile {
			return nativeRuntimeFile{path, gitRawHash([]byte(body)), int64(len(body))}
		}
		view := &nativeRuntimeRoot{parent: parentRoot, root: root, directory: "rt", identity: identity, directories: map[string]bool{"bin": true}, files: []nativeRuntimeFile{pin("bin/real"), pin("bin/tool")}, ready: true}
		if view.verify() == nil {
			t.Fatal("runtime pin followed an in-root symlink")
		}
		view.files = view.files[:1]
		if err := os.Chmod(bin, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(bin, "tool")); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(bin, 0555); err != nil {
			t.Fatal(err)
		}
		if err := view.verify(); err != nil {
			t.Fatal("ordinary runtime pin refused", err)
		}
	})
}

// Procfs, cgroupfs and mount-namespace views cannot hold a planted link, so the
// exact shapes those sites use are exercised against the shared helper.
func TestOpenRootNoFollowKernelViewShapes(t *testing.T) {
	directory, root := rootNoFollowFixture(t)
	for _, name := range []string{"net", "fdinfo"} {
		if err := os.Mkdir(filepath.Join(directory, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	rootNoFollowLink(t, "../real.json", filepath.Join(directory, "net", "dev"))
	rootNoFollowLink(t, "../real.json", filepath.Join(directory, "fdinfo", "3"))
	rootNoFollowLink(t, "real.json", filepath.Join(directory, "status"))
	rootNoFollowLink(t, "real-dir", filepath.Join(directory, "work"))
	rootNoFollowLink(t, "real-dir", filepath.Join(directory, "dev"))
	for _, c := range []struct {
		name string
		flag int
	}{
		{"status", os.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK},
		{"net/dev", os.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK},
		{"fdinfo/3", os.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK},
		{"work", os.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_NONBLOCK},
		{"dev/file.json", unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC},
	} {
		if f, err := openRootNoFollow(root, c.name, c.flag, 0); err == nil {
			f.Close()
			t.Fatal("kernel view shape followed a link", c.name)
		}
	}
	// O_PATH|O_NOFOLLOW returns the link itself; the mount boundary then
	// rejects it because it is neither a directory nor a character device.
	file, err := openRootNoFollow(root, "work", unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFLNK {
		t.Fatal("O_PATH opened the link target", stat.Mode, err)
	}
	// A removed cgroup keeps ENOENT so killOpenedCgroup can prove removal.
	gone := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(gone, 0700); err != nil {
		t.Fatal(err)
	}
	removed, err := os.OpenRoot(gone)
	if err != nil {
		t.Fatal(err)
	}
	defer removed.Close()
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if _, err := openRootNoFollow(removed, "cgroup.kill", os.O_WRONLY|syscall.O_NOFOLLOW, 0); !errors.Is(err, unix.ENOENT) {
		t.Fatal("removed directory lost ENOENT", err)
	}
}

// os.Root.OpenFile silently ignores O_NOFOLLOW for in-root links. Every
// production site that asks for O_NOFOLLOW on a root must use the helper.
// TestRootOpenFileNoFollowSitesUseHelper parses every production file, so a call
// wrapped across lines or with its flags in a variable cannot hide. Every
// OpenFile call on a receiver other than package os must spell its flags as an
// OR of O_* constants, and those flags must not include O_NOFOLLOW: such opens
// belong in openRootNoFollow.
func TestRootOpenFileNoFollowSitesUseHelper(t *testing.T) {
	fset := token.NewFileSet()
	checked := 0
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "OpenFile" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "os" {
				return true
			}
			checked++
			at := fset.Position(call.Pos())
			if len(call.Args) != 3 {
				t.Errorf("%s: OpenFile with %d arguments", at, len(call.Args))
				return true
			}
			names, explicit := openFlagNames(call.Args[1])
			if !explicit {
				t.Errorf("%s: OpenFile flags must be an OR of O_* constants so the symlink rule can be checked", at)
			} else if slices.Contains(names, "O_NOFOLLOW") {
				t.Errorf("%s: OpenFile with O_NOFOLLOW follows in-root symlinks; use openRootNoFollow", at)
			}
			return true
		})
		return nil
	})
	if err != nil || checked == 0 {
		t.Fatal("source scan", err, checked)
	}
}

// openFlagNames returns the O_* constant names an OpenFile flag expression ORs
// together, and false unless the expression is only such constants.
func openFlagNames(expr ast.Expr) ([]string, bool) {
	switch e := expr.(type) {
	case *ast.ParenExpr:
		return openFlagNames(e.X)
	case *ast.BinaryExpr:
		if e.Op != token.OR {
			return nil, false
		}
		left, ok := openFlagNames(e.X)
		if !ok {
			return nil, false
		}
		right, ok := openFlagNames(e.Y)
		return append(left, right...), ok
	case *ast.SelectorExpr:
		pkg, ok := e.X.(*ast.Ident)
		if !ok || (pkg.Name != "os" && pkg.Name != "syscall" && pkg.Name != "unix") || !strings.HasPrefix(e.Sel.Name, "O_") {
			return nil, false
		}
		return []string{e.Sel.Name}, true
	}
	return nil, false
}
