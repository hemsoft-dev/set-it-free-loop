//go:build linux

package reviewer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func gitVMRunFixture(t *testing.T) (*gitVMRun, gitVMOwnerFixture, string) {
	t.Helper()
	f := newGitVMOwnerFixture(t)
	spec, source := gitVMAssetsFixture(t)
	assets, err := openGitVMAssets(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { assets.Close() })
	archiveDirectory := filepath.Join(t.TempDir(), "archive")
	if os.Mkdir(archiveDirectory, 0700) != nil {
		t.Fatal("archive directory")
	}
	archive, err := openGitVMExportArchive(context.Background(), archiveDirectory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { archive.Close(context.Background()) })
	directory, err := os.MkdirTemp("/tmp", "sfl-vm-run-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	run, err := newGitVMRun(context.Background(), f.owner, archive, assets, source, strings.Repeat("c", 32), directory)
	if err != nil {
		t.Fatal(err)
	}
	return run, f, directory
}

func TestGitVMRunUnspentStopAndCloseLeaveNoRecord(t *testing.T) {
	run, f, directory := gitVMRunFixture(t)
	if run.close() == nil {
		t.Fatal("unstopped run closed")
	}
	// A generation the run did not retain cannot drive acquisition.
	run.retainGeneration(&githubReaderGeneration{id: 7})
	if _, err := run.acquire(context.Background(), &gitGenerationReader{generation: &githubReaderGeneration{id: 7}}); err == nil {
		t.Fatal("foreign generation accepted")
	}
	value, err := f.owner.journal.read()
	if err != nil || len(value.Records) != 0 || f.manager.starts != 0 {
		t.Fatal("refused acquisition prepared or started work", value, err)
	}
	if err := run.stop(context.Background()); err != nil {
		t.Fatal("unspent stop", err)
	}
	if err := run.stop(context.Background()); err != nil {
		t.Fatal("repeated stop", err)
	}
	if err := run.close(); err != nil {
		t.Fatal("close", err)
	}
	if _, err := os.Lstat(filepath.Join(directory, "stub.sock")); !os.IsNotExist(err) {
		t.Fatal("stub listener left behind")
	}
	if run.check(context.Background(), gitVMReadTestBinding()) == nil {
		t.Fatal("stopped run admitted a read")
	}
}

func TestGitVMRunRefusesUnsafeConstruction(t *testing.T) {
	run, f, _ := gitVMRunFixture(t)
	run.stop(context.Background())
	run.close()
	directory, err := os.MkdirTemp("/tmp", "sfl-vm-run-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	if os.WriteFile(filepath.Join(directory, "leftover"), []byte("x"), 0600) != nil {
		t.Fatal("leftover")
	}
	if _, err := newGitVMRun(context.Background(), f.owner, run.archive, run.assets, run.source, strings.Repeat("c", 32), directory); err == nil {
		t.Fatal("non-empty run directory accepted")
	}
	empty, err := os.MkdirTemp("/tmp", "sfl-vm-run-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(empty)
	for _, channel := range []string{"", strings.Repeat("g", 32), strings.Repeat("c", 31)} {
		if _, err := newGitVMRun(context.Background(), f.owner, run.archive, run.assets, run.source, channel, empty); err == nil {
			t.Fatal("invalid channel accepted", channel)
		}
	}
	long := "/tmp/" + strings.Repeat("d", 100)
	if _, err := newGitVMRun(context.Background(), f.owner, run.archive, run.assets, run.source, strings.Repeat("c", 32), long); err == nil {
		t.Fatal("overlong endpoint accepted")
	}
}
