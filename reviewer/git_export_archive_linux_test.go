//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func exportArchiveFixture(t *testing.T) (*gitExportArchive, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "archive")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	a, err := openGitExportArchive(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	return a, dir
}
func TestGitExportArchiveActualGitSurvivesFreshProcesses(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			raw, binding, store := gitMaterializationFixture(t, format)
			original, err := CaptureSnapshotEvidence(context.Background(), store, binding.Snapshot)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]Object{}
			for _, revision := range gitMaterializationRevisions(binding.Snapshot) {
				for _, path := range original.Paths() {
					object, err := store.Read(context.Background(), revision, path, snapshotObjectBytes)
					if err == nil {
						want[revision+":"+path] = object
					}
				}
			}
			// The cache and new process cannot recover by opening the old quarantine.
			if err = os.RemoveAll(store.GitDir); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(t.TempDir(), "archive")
			if err = os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			runExportArchiveChild(t, "stage", dir, raw, binding, "")
			a, err := openGitExportArchive(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			m, err := a.reopen(context.Background(), binding)
			if err != nil {
				t.Fatal(err)
			}
			job := testJob()
			job.Snapshot = binding.Snapshot
			job.Packet = m.packet
			bound, err := restoreGitMaterialization(context.Background(), m, job)
			if err != nil || bound.evidence.Packet() != original.Packet() || !reflect.DeepEqual(bound.evidence.Coverage(), original.Coverage()) {
				t.Fatal("persisted packet changed", err)
			}
			for _, revision := range gitMaterializationRevisions(binding.Snapshot) {
				for _, path := range original.Paths() {
					object, err := m.Read(context.Background(), revision, path, snapshotObjectBytes)
					expected, exists := want[revision+":"+path]
					if exists && (err != nil || !reflect.DeepEqual(object, expected)) {
						t.Fatal("plain object changed", revision, path, err)
					}
					if !exists && err == nil {
						t.Fatal("nonfile path readable", path)
					}
				}
			}
			payload := filepath.Join(dir, gitExportDirectory(binding.Attempt), "payload.bin")
			info, err := os.Stat(payload)
			if err != nil || info.Mode().Perm() != 0400 || info.Size() != int64(len(raw)) {
				t.Fatal(info, err)
			}
			m.Close()
			if err = a.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			runExportArchiveChild(t, "reopen", dir, nil, binding, "")
		})
	}
}

func TestGitExportArchiveRefusesMutatedOrForeignRetention(t *testing.T) {
	for _, kind := range []string{"wire-hash", "packet-hash", "payload-byte", "truncated", "trailing", "writable", "symlink", "hardlink", "unknown-file", "duplicate-json", "foreign-binding", "record-state", "lock-replaced", "root-replaced"} {
		t.Run(kind, func(t *testing.T) {
			raw, binding, _ := gitMaterializationFixture(t, "sha1")
			a, dir := exportArchiveFixture(t)
			m, err := a.stage(context.Background(), binding, io.NopCloser(bytes.NewReader(raw)))
			if err != nil {
				t.Fatal(err)
			}
			m.Close()
			attempt := filepath.Join(dir, gitExportDirectory(binding.Attempt))
			payload := filepath.Join(attempt, "payload.bin")
			metadata := filepath.Join(attempt, "archive.json")
			var record gitExportRecord
			data, err := os.ReadFile(metadata)
			if err != nil || json.Unmarshal(data, &record) != nil {
				t.Fatal(err)
			}
			rewrite := func() {
				b, _ := json.Marshal(record)
				if err := os.WriteFile(metadata, b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "wire-hash":
				record.WireHash = strings.Repeat("0", 64)
				rewrite()
			case "packet-hash":
				record.PacketHash = strings.Repeat("0", 64)
				rewrite()
			case "payload-byte", "truncated", "trailing":
				changed := append([]byte{}, raw...)
				switch kind {
				case "payload-byte":
					changed[len(changed)-1] ^= 1
				case "truncated":
					changed = changed[:len(changed)-1]
				case "trailing":
					changed = append(changed, 0)
				}
				if err := os.Chmod(payload, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(payload, changed, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(payload, 0400); err != nil {
					t.Fatal(err)
				}
			case "writable":
				if err := os.Chmod(payload, 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(t.TempDir(), "foreign")
				if err := os.WriteFile(target, raw, 0400); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(payload); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, payload); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(payload, filepath.Join(t.TempDir(), "extra-link")); err != nil {
					t.Fatal(err)
				}
			case "unknown-file":
				if err := os.WriteFile(filepath.Join(attempt, "foreign"), []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			case "duplicate-json":
				if err := os.WriteFile(metadata, append([]byte(`{"Version":1,`), data[1:]...), 0600); err != nil {
					t.Fatal(err)
				}
			case "foreign-binding":
				binding.ReleaseHash = Digest("other release")
			case "record-state":
				record.State = "receiving"
				record.WireHash = ""
				record.PacketHash = ""
				record.Bytes = 0
				rewrite()
			case "lock-replaced":
				if err := os.Rename(filepath.Join(dir, "git-export.lock"), filepath.Join(dir, "old-lock")); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "git-export.lock"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "root-replaced":
				if err := os.Rename(dir, dir+"-owned"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "foreign"), []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			parent, err := filepath.EvalSymlinks(filepath.Dir(dir))
			if err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(parent, filepath.Base(dir))
			watch := gitWatchFDs(t, gitFDTargets(gitMaterializationBlobTarget, archive+"/", archive+"-owned/"))
			bad, err := a.reopen(context.Background(), binding)
			if bad != nil {
				bad.Close()
			}
			if err == nil || bad != nil {
				t.Fatal("retention mutation granted", kind, err)
			}
			if leaked := watch.leaked(t); len(leaked) != 0 {
				t.Fatal("failed reopen leaked sealed blobs", leaked)
			}
			if kind == "root-replaced" {
				b, err := os.ReadFile(filepath.Join(dir, "foreign"))
				if err != nil || string(b) != "preserve" {
					t.Fatal("foreign root changed", err)
				}
			}
		})
	}
}

type exportBlockingInput struct {
	prefix          *bytes.Reader
	blocked, closed chan struct{}
	once            sync.Once
}

func (b *exportBlockingInput) Read(p []byte) (int, error) {
	if b.prefix.Len() > 0 {
		return b.prefix.Read(p)
	}
	b.once.Do(func() { close(b.blocked) })
	<-b.closed
	return 0, io.ErrClosedPipe
}
func (b *exportBlockingInput) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}
func TestGitExportArchiveCancellationCloseAndNoAutomaticRetry(t *testing.T) {
	raw, binding, _ := gitMaterializationFixture(t, "sha1")
	a, dir := exportArchiveFixture(t)
	input := &exportBlockingInput{prefix: bytes.NewReader(raw[:len(raw)/2]), blocked: make(chan struct{}), closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		m, err := a.stage(ctx, binding, input)
		if m != nil {
			m.Close()
		}
		done <- err
	}()
	select {
	case <-input.blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("intake never blocked")
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer closeCancel()
	if err := a.Close(closeCtx); !errors.Is(err, errGitExportArchive) {
		t.Fatal("close dropped active ownership", err)
	}
	otherCtx, otherCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer otherCancel()
	if other, err := openGitExportArchive(otherCtx, dir); err == nil || other != nil {
		if other != nil {
			other.Close(context.Background())
		}
		t.Fatal("active archive lock escaped")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled intake succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled intake did not join")
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := openGitExportArchive(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(context.Background())
	if m, err := reopened.reopen(context.Background(), binding); err == nil || m != nil {
		t.Fatal("partial export reopened", err)
	}
	for _, attempt := range []string{binding.Attempt, strings.Repeat("b", 32)} {
		b := binding
		b.Attempt = attempt
		if m, err := reopened.stage(context.Background(), b, io.NopCloser(bytes.NewReader(raw))); err == nil || m != nil {
			t.Fatal("partial attempt silently retried", err)
		}
	}
	payload, err := os.ReadFile(filepath.Join(dir, gitExportDirectory(binding.Attempt), "payload.bin"))
	if err != nil || !bytes.Equal(payload, raw[:len(raw)/2]) {
		t.Fatal("partial evidence changed", err)
	}
}

func runExportArchiveChild(t *testing.T, mode, dir string, raw []byte, binding gitMaterializationBinding, signal string) {
	t.Helper()
	command := exportArchiveChild(t, mode, dir, raw, binding, signal)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("archive child %s: %v: %s", mode, err, output)
	}
}
func exportArchiveChild(t *testing.T, mode, dir string, raw []byte, binding gitMaterializationBinding, signal string) *exec.Cmd {
	t.Helper()
	root := t.TempDir()
	data := filepath.Join(root, "wire")
	if raw != nil {
		if err := os.WriteFile(data, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := json.Marshal(binding)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGitExportArchiveHelperProcess$", "-test.timeout=8s")
	cmd.Env = []string{"SFL_TEST_GIT_EXPORT_CHILD=1", "SFL_TEST_GIT_EXPORT_MODE=" + mode, "SFL_TEST_GIT_EXPORT_DIRECTORY=" + dir, "SFL_TEST_GIT_EXPORT_DATA=" + data, "SFL_TEST_GIT_EXPORT_BINDING=" + string(b), "SFL_TEST_GIT_EXPORT_SIGNAL=" + signal, "TMPDIR=" + root, "GORACE=atexit_sleep_ms=0"}
	return cmd
}
func TestGitExportArchiveHelperProcess(t *testing.T) {
	if os.Getenv("SFL_TEST_GIT_EXPORT_CHILD") != "1" {
		return
	}
	var binding gitMaterializationBinding
	if err := json.Unmarshal([]byte(os.Getenv("SFL_TEST_GIT_EXPORT_BINDING")), &binding); err != nil {
		t.Fatal(err)
	}
	a, err := openGitExportArchive(context.Background(), os.Getenv("SFL_TEST_GIT_EXPORT_DIRECTORY"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	mode := os.Getenv("SFL_TEST_GIT_EXPORT_MODE")
	if mode == "reopen" {
		m, err := a.reopen(context.Background(), binding)
		if err != nil {
			t.Fatal(err)
		}
		defer m.Close()
		job := testJob()
		job.Snapshot = binding.Snapshot
		job.Packet = m.packet
		bound, err := restoreGitMaterialization(context.Background(), m, job)
		if err != nil {
			t.Fatal(err)
		}
		reader, err := bound.Reader(binding.Snapshot)
		if err != nil {
			t.Fatal(err)
		}
		text, err := reader.Read(context.Background(), binding.Snapshot.Head, "safe.go")
		if err != nil || !strings.Contains(text, "FOUR") {
			t.Fatal("fresh process source", err)
		}
		return
	}
	raw, err := os.ReadFile(os.Getenv("SFL_TEST_GIT_EXPORT_DATA"))
	if err != nil {
		t.Fatal(err)
	}
	var input io.ReadCloser = io.NopCloser(bytes.NewReader(raw))
	if mode == "block" {
		input = &exportChildBlockingInput{prefix: bytes.NewReader(raw[:len(raw)/2]), signal: os.Getenv("SFL_TEST_GIT_EXPORT_SIGNAL")}
	}
	m, err := a.stage(context.Background(), binding, input)
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
}

type exportChildBlockingInput struct {
	prefix *bytes.Reader
	signal string
}

func (b *exportChildBlockingInput) Read(p []byte) (int, error) {
	if b.prefix.Len() > 0 {
		return b.prefix.Read(p)
	}
	if err := os.WriteFile(b.signal, []byte("blocked"), 0600); err != nil {
		return 0, err
	}
	select {}
}
func (b *exportChildBlockingInput) Close() error { return nil }
func TestGitExportArchiveKilledControllerPreservesUnknownIntake(t *testing.T) {
	raw, binding, _ := gitMaterializationFixture(t, "sha1")
	dir := filepath.Join(t.TempDir(), "archive")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	signal := filepath.Join(t.TempDir(), "blocked")
	child := exportArchiveChild(t, "block", dir, raw, binding, signal)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Process.Kill()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(signal); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("archive child never reached unknown intake")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err == nil {
		t.Fatal("child was not killed")
	}
	status, ok := child.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatal("no actual controller death evidence", child.ProcessState)
	}
	a, err := openGitExportArchive(context.Background(), dir)
	if err != nil {
		t.Fatal("dead controller retained lock", err)
	}
	defer a.Close(context.Background())
	if m, err := a.reopen(context.Background(), binding); err == nil || m != nil {
		t.Fatal("killed intake was publishable", err)
	}
	fresh := binding
	fresh.Attempt = strings.Repeat("b", 32)
	if m, err := a.stage(context.Background(), fresh, io.NopCloser(bytes.NewReader(raw))); err == nil || m != nil {
		t.Fatal("unknown controller intake permitted refetch", err)
	}
}

type exportPausedInput struct {
	prefix, suffix *bytes.Reader
	paused, resume chan struct{}
	once           sync.Once
}

func (r *exportPausedInput) Read(p []byte) (int, error) {
	if r.prefix.Len() > 0 {
		return r.prefix.Read(p)
	}
	r.once.Do(func() { close(r.paused) })
	<-r.resume
	return r.suffix.Read(p)
}
func (r *exportPausedInput) Close() error { return nil }
func TestGitExportArchiveStageRefusesReplacedAttemptDirectory(t *testing.T) {
	raw, binding, _ := gitMaterializationFixture(t, "sha1")
	a, dir := exportArchiveFixture(t)
	input := &exportPausedInput{prefix: bytes.NewReader(raw[:len(raw)/2]), suffix: bytes.NewReader(raw[len(raw)/2:]), paused: make(chan struct{}), resume: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		m, err := a.stage(context.Background(), binding, input)
		if m != nil {
			m.Close()
		}
		done <- err
	}()
	select {
	case <-input.paused:
	case <-time.After(3 * time.Second):
		t.Fatal("no paused intake")
	}
	attempt := filepath.Join(dir, gitExportDirectory(binding.Attempt))
	if err := os.Rename(attempt, attempt+"-owned"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(attempt, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attempt, "foreign"), []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	close(input.resume)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("replaced attempt accepted")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("intake failed to join")
	}
	b, err := os.ReadFile(filepath.Join(attempt, "foreign"))
	if err != nil || string(b) != "preserve" {
		t.Fatal("foreign attempt modified", err)
	}
	if _, err := os.Stat(filepath.Join(attempt, "archive.json")); !os.IsNotExist(err) {
		t.Fatal("metadata escaped anchored directory", err)
	}
}
