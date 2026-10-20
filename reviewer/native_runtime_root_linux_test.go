//go:build linux

package reviewer

import (
	"bytes"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func nativeRuntimePrivateParent(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	if os.Chmod(parent, 0700) != nil {
		t.Fatal("private parent fixture")
	}
	return parent
}

func TestNativeRuntimePreparesOnlyVerifiedReadOnlyFilesAndRemovesOwnedTree(t *testing.T) {
	m, body := nativeRuntimeFixture(t)
	parent := nativeRuntimePrivateParent(t)
	r, err := prepareNativeRuntime(parent, bytes.NewReader(nativeRuntimeFrame(t, m, body)), m.Program)
	if err != nil {
		t.Fatal(err)
	}
	for _, pin := range m.Files {
		path := filepath.Join(parent, nativeRuntimeDirectory, pin.Path)
		raw, err := os.ReadFile(path)
		info, statErr := os.Lstat(path)
		if err != nil || statErr != nil || authHash(raw) != pin.SHA256 || info.Mode().Perm()&0222 != 0 {
			t.Fatal("prepared file differs", err, statErr)
		}
	}
	fd, err := r.directoryFD()
	if err != nil {
		t.Fatal(err)
	}
	fd.Close()
	if r.Close() != nil || r.Close() != nil {
		t.Fatal("owned cleanup failed")
	}
	if _, err := os.Lstat(filepath.Join(parent, nativeRuntimeDirectory)); !os.IsNotExist(err) {
		t.Fatal("prepared root retained", err)
	}
}
func TestNativeRuntimeRejectsCorruptBodiesWithoutRetainingPartialTree(t *testing.T) {
	m, body := nativeRuntimeFixture(t)
	frame := nativeRuntimeFrame(t, m, body)
	frame[len(frame)-1] ^= 1
	parent := nativeRuntimePrivateParent(t)
	if _, err := prepareNativeRuntime(parent, bytes.NewReader(frame), m.Program); err == nil {
		t.Fatal("corrupt archive accepted")
	}
	if _, err := os.Lstat(filepath.Join(parent, nativeRuntimeDirectory)); !os.IsNotExist(err) {
		t.Fatal("partial root retained", err)
	}
}
func TestNativeRuntimeNeverAdoptsOrDeletesReplacedDirectory(t *testing.T) {
	m, body := nativeRuntimeFixture(t)
	frame := nativeRuntimeFrame(t, m, body)
	parent := nativeRuntimePrivateParent(t)
	path := filepath.Join(parent, nativeRuntimeDirectory)
	if os.Mkdir(path, 0700) != nil {
		t.Fatal("setup")
	}
	if _, err := prepareNativeRuntime(parent, bytes.NewReader(frame), m.Program); err == nil {
		t.Fatal("existing root adopted")
	}
	if os.Remove(path) != nil {
		t.Fatal("setup")
	}
	r, err := prepareNativeRuntime(parent, bytes.NewReader(frame), m.Program)
	if err != nil {
		t.Fatal(err)
	}
	if os.Rename(path, path+"-retained") != nil || os.Mkdir(path, 0700) != nil {
		t.Fatal("setup")
	}
	marker := filepath.Join(path, "unrelated")
	if os.WriteFile(marker, []byte("preserve"), 0600) != nil {
		t.Fatal("setup")
	}
	if r.Close() == nil {
		t.Fatal("replaced root deleted")
	}
	if raw, err := os.ReadFile(marker); err != nil || string(raw) != "preserve" {
		t.Fatal("unrelated content changed", err)
	}
	// The retained read-only fixture belongs to the test and needs permissions
	// restored for testing.T's cleanup; production Close deliberately preserves it.
	retained := path + "-retained"
	filepath.WalkDir(retained, func(path string, entry os.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			return os.Chmod(path, 0700)
		}
		return err
	})
}
func TestNativeRuntimeDetectsChangedBytesAndUnexpectedFilesBeforeDescriptor(t *testing.T) {
	m, body := nativeRuntimeFixture(t)
	for _, extra := range []bool{false, true} {
		parent := nativeRuntimePrivateParent(t)
		r, err := prepareNativeRuntime(parent, bytes.NewReader(nativeRuntimeFrame(t, m, body)), m.Program)
		if err != nil {
			t.Fatal(err)
		}
		if extra {
			root := filepath.Join(parent, nativeRuntimeDirectory)
			os.Chmod(root, 0700)
			os.WriteFile(filepath.Join(root, "unapproved"), []byte("inert"), 0400)
			os.Chmod(root, 0555)
		} else {
			file := filepath.Join(parent, nativeRuntimeDirectory, m.Files[0].Path)
			os.Chmod(file, 0600)
			os.WriteFile(file, []byte("changed"), 0400)
			os.Chmod(file, 0444)
		}
		if fd, err := r.directoryFD(); err == nil {
			fd.Close()
			t.Fatal("mutated root exposed")
		}
		if r.Close() != nil {
			t.Fatal("cleanup failed")
		}
	}
}

func TestNativeRuntimeRejectsOpenFIFOWithoutWaitingForItsWriter(t *testing.T) {
	m, body := nativeRuntimeFixture(t)
	parent := nativeRuntimePrivateParent(t)
	r, err := prepareNativeRuntime(parent, bytes.NewReader(nativeRuntimeFrame(t, m, body)), m.Program)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(parent, nativeRuntimeDirectory, "firmware")
	path := filepath.Join(parent, nativeRuntimeDirectory, m.Files[0].Path)
	if os.Chmod(directory, 0700) != nil || os.Remove(path) != nil || unix.Mkfifo(path, 0600) != nil || os.Chmod(directory, 0555) != nil {
		t.Fatal("FIFO setup")
	}
	keepalive, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		fd, err := r.directoryFD()
		if fd != nil {
			fd.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		unix.Close(keepalive)
		if err == nil {
			t.Error("FIFO accepted")
		}
	case <-time.After(200 * time.Millisecond):
		unix.Close(keepalive)
		<-done
		t.Error("metadata-invalid FIFO waited for its writer")
	}
	if r.Close() != nil {
		t.Fatal("cleanup failed")
	}
}
