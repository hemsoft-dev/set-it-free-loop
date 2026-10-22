//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitVMArchiveFixture(t *testing.T) (gitVMRequest, []byte) {
	t.Helper()
	_, binding, store := gitMaterializationFixture(t, "sha1")
	q := gitVMRequestFixture(t, binding)
	var wire bytes.Buffer
	if err := writeGitVMExport(context.Background(), &wire, store, q); err != nil {
		t.Fatal(err)
	}
	return q, wire.Bytes()
}

func gitVMArchiveTestDir(t *testing.T) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "archive")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func openGitVMArchiveTest(t *testing.T, directory string) *gitVMExportArchive {
	t.Helper()
	a, err := openGitVMExportArchive(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return a
}

func TestGitVMArchiveActualProducerSurvivesReopen(t *testing.T) {
	q, wire := gitVMArchiveFixture(t)
	directory := gitVMArchiveTestDir(t)
	a := openGitVMArchiveTest(t, directory)
	m, err := a.stage(context.Background(), q, io.NopCloser(bytes.NewReader(wire)))
	if err != nil {
		t.Fatal(err)
	}
	wantPacket := m.packet
	m.Close()
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	a = openGitVMArchiveTest(t, directory)
	m, err = a.reopen(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	object, err := m.Read(context.Background(), q.Binding.Snapshot.Head, "safe.go", snapshotObjectBytes)
	if err != nil || !strings.Contains(string(object.Data), "FOUR") || m.packet != wantPacket {
		t.Fatal("retained source differs", err)
	}
	if next, err := a.stage(context.Background(), q, io.NopCloser(bytes.NewReader(wire))); err == nil {
		next.Close()
		t.Fatal("replayed original attempt")
	}
	changed := q
	changed.Binding.ReleaseHash = Digest("different release")
	if m, err := a.reopen(context.Background(), changed); err == nil {
		m.Close()
		t.Fatal("changed request reopened")
	}
}

func TestGitVMArchiveRepairsOnlyLostStagingAcknowledgement(t *testing.T) {
	q, wire := gitVMArchiveFixture(t)
	directory := gitVMArchiveTestDir(t)
	a := openGitVMArchiveTest(t, directory)
	m, err := a.stage(context.Background(), q, io.NopCloser(bytes.NewReader(wire)))
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
	// Simulate process loss after the inner fsync but before the outer fsync.
	j, err := a.journal()
	if err != nil {
		t.Fatal(err)
	}
	j.Records[0].State = "receiving"
	if a.writeJournal(j) != nil || a.Close(context.Background()) != nil {
		t.Fatal("prepare recovery")
	}
	a = openGitVMArchiveTest(t, directory)
	m, err = a.reopen(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
	j, err = a.journal()
	if err != nil || j.Records[0].State != "staged" {
		t.Fatal("staging acknowledgement not restored", err)
	}
}

func TestGitVMArchiveIncompleteAttemptsRemainOwned(t *testing.T) {
	q, wire := gitVMArchiveFixture(t)
	for _, data := range [][]byte{wire[:2], wire[:len(wire)-1], append(append([]byte{}, wire...), 0)} {
		directory := gitVMArchiveTestDir(t)
		a := openGitVMArchiveTest(t, directory)
		if m, err := a.stage(context.Background(), q, io.NopCloser(bytes.NewReader(data))); err == nil {
			m.Close()
			t.Fatal("partial export accepted")
		}
		if err := a.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		a = openGitVMArchiveTest(t, directory)
		j, err := a.journal()
		if err != nil || len(j.Records) != 1 || j.Records[0].State != "receiving" {
			t.Fatal("lost incomplete ownership", err)
		}
		if m, err := a.reopen(context.Background(), q); err == nil {
			m.Close()
			t.Fatal("partial archive became staged")
		}
		next := q
		next.Binding.Attempt = strings.Repeat("c", 32)
		if m, err := a.stage(context.Background(), next, io.NopCloser(bytes.NewReader(wire))); err == nil {
			m.Close()
			t.Fatal("unknown attempt allowed new intake")
		}
	}
}

func TestGitVMArchiveHeaderCancellationAndQueuedClose(t *testing.T) {
	q, _ := gitVMArchiveFixture(t)
	a := openGitVMArchiveTest(t, gitVMArchiveTestDir(t))
	reader, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := a.stage(ctx, q, reader); done <- err }()
	// The first bytes are read only after the durable receiving intent exists.
	if _, err := writer.Write([]byte{0, 0}); err != nil {
		t.Fatal(err)
	}
	short, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	if a.Close(short) == nil {
		t.Fatal("closed owner while intake still active")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled intake succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stalled header did not stop")
	}
	if a.verify() != nil {
		t.Fatal("cancelled close destroyed ownership")
	}
}

func TestGitVMArchiveRefusesPurposePayloadAndRootTampering(t *testing.T) {
	q, wire := gitVMArchiveFixture(t)
	t.Run("host archive", func(t *testing.T) {
		directory := gitVMArchiveTestDir(t)
		raw, err := openGitExportArchive(context.Background(), directory)
		if err != nil {
			t.Fatal(err)
		}
		raw.Close(context.Background())
		if a, err := openGitVMExportArchive(context.Background(), directory); err == nil {
			a.Close(context.Background())
			t.Fatal("adopted host archive")
		}
		raw, err = openGitExportArchive(context.Background(), directory)
		if err != nil {
			t.Fatal("refused source opener damaged host archive", err)
		}
		raw.Close(context.Background())
	})
	t.Run("purpose", func(t *testing.T) {
		directory := gitVMArchiveTestDir(t)
		a := openGitVMArchiveTest(t, directory)
		if writeAuth(a.held.root, "source-vm.json", []byte(`{"Version":1,"Purpose":"native-review","Records":[]}`)) != nil {
			t.Fatal("tamper")
		}
		if a.verify() == nil {
			t.Fatal("accepted another purpose")
		}
	})
	t.Run("payload", func(t *testing.T) {
		a := openGitVMArchiveTest(t, gitVMArchiveTestDir(t))
		m, err := a.stage(context.Background(), q, io.NopCloser(bytes.NewReader(wire)))
		if err != nil {
			t.Fatal(err)
		}
		m.Close()
		payload := filepath.Join(a.raw.directory, gitExportDirectory(q.Binding.Attempt), "payload.bin")
		if os.Chmod(payload, 0600) != nil {
			t.Fatal("chmod")
		}
		f, err := os.OpenFile(payload, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.WriteAt([]byte("bad"), 0)
		f.Close()
		if err != nil || os.Chmod(payload, 0400) != nil {
			t.Fatal("tamper", err)
		}
		if m, err := a.reopen(context.Background(), q); err == nil {
			m.Close()
			t.Fatal("tampered payload reopened")
		}
	})
	t.Run("replacement root", func(t *testing.T) {
		parent := t.TempDir()
		directory := filepath.Join(parent, "archive")
		if os.Mkdir(directory, 0700) != nil {
			t.Fatal("mkdir")
		}
		a := openGitVMArchiveTest(t, directory)
		if os.Rename(directory, directory+"-original") != nil || os.Mkdir(directory, 0700) != nil {
			t.Fatal("replace")
		}
		if m, err := a.stage(context.Background(), q, io.NopCloser(bytes.NewReader(wire))); err == nil {
			m.Close()
			t.Fatal("replacement admitted")
		}
		entries, err := os.ReadDir(directory)
		if err != nil || len(entries) != 0 {
			t.Fatal("replacement modified", err)
		}
	})
}

func TestGitVMArchiveExcludesConcurrentOwner(t *testing.T) {
	directory := gitVMArchiveTestDir(t)
	_ = openGitVMArchiveTest(t, directory)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if a, err := openGitVMExportArchive(ctx, directory); err == nil {
		a.Close(context.Background())
		t.Fatal("second owner acquired archive")
	}
}

func TestGitVMArchiveFreshProcessRecovery(t *testing.T) {
	if directory := os.Getenv("SFL_GIT_VM_ARCHIVE_CHILD"); directory != "" {
		request, err := os.ReadFile(filepath.Join(directory, "request.json"))
		if err != nil {
			t.Fatal(err)
		}
		q, err := DecodeStrict[gitVMRequest](request, gitVMRecordBytes)
		if err != nil || !q.valid() {
			t.Fatal("invalid fixture request")
		}
		wire, err := os.Open(filepath.Join(directory, "wire.bin"))
		if err != nil {
			t.Fatal(err)
		}
		a, err := openGitVMExportArchive(context.Background(), filepath.Join(directory, "archive"))
		if err != nil {
			t.Fatal(err)
		}
		m, err := a.stage(context.Background(), q, wire)
		if err != nil {
			t.Fatal(err)
		}
		m.Close()
		if os.Getenv("SFL_GIT_VM_ARCHIVE_ACK_LOSS") == "1" {
			j, err := a.journal()
			if err != nil {
				t.Fatal(err)
			}
			j.Records[0].State = "receiving"
			if a.writeJournal(j) != nil {
				t.Fatal("lost acknowledgement fixture")
			}
		}
		// Leave the owner, roots and flocks open. The new process must recover
		// persisted state, not rely on orderly Close or shared Go memory.
		os.Exit(0)
	}
	q, wire := gitVMArchiveFixture(t)
	for _, lostAck := range []string{"0", "1"} {
		t.Run(lostAck, func(t *testing.T) {
			parent := t.TempDir()
			request, err := json.Marshal(q)
			if err != nil {
				t.Fatal(err)
			}
			if os.WriteFile(filepath.Join(parent, "request.json"), request, 0600) != nil ||
				os.WriteFile(filepath.Join(parent, "wire.bin"), wire, 0600) != nil ||
				os.Mkdir(filepath.Join(parent, "archive"), 0700) != nil {
				t.Fatal("fixture preparation")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGitVMArchiveFreshProcessRecovery$", "-test.count=1")
			cmd.Env = append(os.Environ(), "SFL_GIT_VM_ARCHIVE_CHILD="+parent, "SFL_GIT_VM_ARCHIVE_ACK_LOSS="+lostAck)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("child %v: %s", err, output)
			}
			a := openGitVMArchiveTest(t, filepath.Join(parent, "archive"))
			m, err := a.reopen(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			object, err := m.Read(context.Background(), q.Binding.Snapshot.Head, "safe.go", snapshotObjectBytes)
			if err != nil || !strings.Contains(string(object.Data), "FOUR") {
				t.Fatal("fresh-process source differs", err)
			}
		})
	}
}

func TestGitVMArchiveWaitingOwnerRefusesReplacedLockBeforeCleanup(t *testing.T) {
	directory := gitVMArchiveTestDir(t)
	a := openGitVMArchiveTest(t, directory)
	original, err := a.held.file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		other, err := openGitVMExportArchive(ctx, directory)
		if other != nil {
			other.Close(context.Background())
		}
		finished <- err
	}()
	// Observe two descriptors of the original lock before replacing its name.
	deadline := time.Now().Add(time.Second)
	for {
		fds, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, fd := range fds {
			info, err := os.Stat(filepath.Join("/proc/self/fd", fd.Name()))
			if err == nil && os.SameFile(info, original) {
				count++
			}
		}
		if count >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second owner did not capture original lock")
		}
		time.Sleep(time.Millisecond)
	}
	lock := filepath.Join(directory, "refresh.lock")
	if os.Remove(lock) != nil || os.WriteFile(lock, nil, 0600) != nil {
		t.Fatal("replace lock")
	}
	marker := filepath.Join(directory, ".replace-"+strings.Repeat("a", 32))
	if os.WriteFile(marker, []byte("replacement owner's pending write"), 0600) != nil {
		t.Fatal("marker")
	}
	if a.Close(context.Background()) != nil {
		t.Fatal("release original lock")
	}
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("replacement lock admitted")
		}
	case <-time.After(time.Second):
		t.Fatal("waiting owner stuck")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "replacement owner's pending write" {
		t.Fatal("old lock holder cleaned replacement work", err)
	}
}
