//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Explicit model-free fixture only. The outer diagnostic unit must bound the
// test and QEMU. This proves the guest data path, not a production host launch,
// durable recovery, provider eligibility, auth release or trusted promotion.
func TestGitVMGuestContainedActualGit(t *testing.T) {
	directory := os.Getenv("SFL_SOURCE_GUEST_IMAGE")
	if directory == "" {
		t.Skip("explicit source-only image and bounded diagnostic unit required")
	}
	fixtureCase := os.Getenv("SFL_SOURCE_GUEST_CASE")
	if fixtureCase == "" {
		fixtureCase = "normal"
	}
	switch fixtureCase {
	case "normal", "export-close", "control-close", "read-close", "foreign-request":
	default:
		t.Fatal("unknown fixture case")
	}
	spec, source, release := gitVMActualImageFixture(t, directory)
	store, snapshot := gitQuarantineFixture(t)
	if fixtureCase == "export-close" {
		f := newSnapshotGitFixture(t)
		f.write(t, "large.go", strings.Repeat("synthetic source line\n", 80000))
		f.run(t, "add", "large.go")
		f.run(t, "commit", "--quiet", "-m", "bounded export backpressure fixture")
		f.snapshot.Head = f.run(t, "rev-parse", "HEAD")
		store, snapshot = gitQuarantineFromFixture(t, f, "sha1")
	}
	source.Binding.Snapshot = snapshot
	readBinding := gitVMReadTestBinding()
	readBinding.Source = source.Binding
	q := gitVMGuestRequest{1, gitVMGuestPurpose, source, readBinding}
	if !q.valid() || !release.matches(q) {
		t.Fatal("image/request mismatch")
	}
	if fixtureCase == "export-close" {
		counter := &gitVMGuestExportWriter{ctx: context.Background(), target: io.Discard, remaining: gitExportWireBytes + gitVMRecordBytes + 4}
		if err := writeGitVMExport(context.Background(), counter, store, q.Source); err != nil || counter.bytes <= 1<<20 {
			t.Fatal("large actual export required", err, counter.bytes)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	var channels []net.Conn
	var peers []*os.File
	for i := 0; i < 3; i++ {
		pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}

		if fixtureCase == "export-close" && i == 2 {
			if unix.SetsockoptInt(pair[1], unix.SOL_SOCKET, unix.SO_SNDBUF, 4096) != nil {
				unix.Close(pair[0])
				unix.Close(pair[1])
				t.Fatal("export backpressure buffer")
			}
		}
		host := os.NewFile(uintptr(pair[0]), "fixture-host")
		conn, err := net.FileConn(host)
		host.Close()
		if err != nil {
			unix.Close(pair[1])
			t.Fatal(err)
		}
		channels = append(channels, conn)
		peers = append(peers, os.NewFile(uintptr(pair[1]), "fixture-guest"))
	}
	defer func() {
		for _, c := range channels {
			c.Close()
		}
		closeVMFiles(peers)
	}()
	assets, err := openGitVMAssets(ctx, spec)
	if err != nil {
		t.Fatal("source assets", err)
	}
	defer assets.Close()
	boot, err := assets.duplicate(ctx, q.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer closeVMFiles(boot)
	kvm, err := openVMKVM()
	if err != nil {
		t.Fatal(err)
	}
	defer kvm.Close()
	parent := os.Getenv("SFL_SOURCE_GUEST_RUN_DIR")
	if !sourcePath(parent) || privateDirectory(parent) != nil {
		t.Fatal("private fixture runtime parent required")
	}
	prepared, err := prepareNativeRuntime(parent, io.NewSectionReader(boot[4], 0, spec.Manifest.QEMURuntime.Bytes), spec.Manifest.QEMU, io.NewSectionReader(boot[0], 0, spec.Manifest.QEMU.Bytes+1))
	if err != nil {
		t.Fatal("captured QEMU runtime", err)
	}
	// Keep the measured files as local evidence. Only close management handles;
	// the outer runner separately proves whole-unit shutdown before disposal.
	defer prepared.root.Close()
	defer prepared.parent.Close()
	runtimeRoot, err := prepared.directoryFD()
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeRoot.Close()
	serial, serialChild, err := nativeBootDiagnosticChannel()
	if err != nil {
		t.Fatal(err)
	}
	defer serialChild.Close()
	serialDone := make(chan struct{})
	go func() { defer close(serialDone); _, _ = io.Copy(os.Stdout, io.LimitReader(serial, 64<<10)) }()
	defer func() { serial.Close(); <-serialDone }()
	cmd := exec.CommandContext(ctx, "/proc/self/fd/12", nativeRuntimeNamespaceArgs(gitVMSourceQEMUArgs(q))...)
	cmd.Args[0] = "sfl-source-fixture-bootstrap"
	cmd.Env = []string{"LANG=C", "LC_ALL=C"}
	cmd.Dir = "/"
	cmd.ExtraFiles = gitVMLaunchFiles([]*os.File{boot[0], boot[1], boot[2], kvm, boot[3], boot[4], boot[5], runtimeRoot}, peers, serialChild)
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	cmd.WaitDelay = 2 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	closeVMFiles(cmd.ExtraFiles)
	peers = nil
	exited := false
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		cancel()
		if exited {
			return
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("fixture QEMU did not exit")
		}
	}()
	stop := gitBrokerAfterFunc(ctx, func() {
		for _, c := range channels {
			c.Close()
		}
	})
	defer stop()
	readActive := make(chan struct{})
	readClosed := make(chan struct{})
	var upstreamCalls atomic.Int32
	readDone := make(chan error, 1)
	go func() {
		_, err := serveGitVMRead(ctx, channels[1], q.Read, gitExchangeFunc(func(ctx context.Context, r GitHubGitReadRequest) (io.ReadCloser, error) {
			upstreamCalls.Add(1)
			args := []string{"upload-pack", "--stateless-rpc"}
			if r.Operation == GitHubGitAdvertisement {
				args = append(args, "--advertise-refs")
			}
			args = append(args, store.GitDir)
			git := exec.CommandContext(ctx, store.Git, args...)
			git.Env = []string{"PATH=/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "LC_ALL=C"}
			if r.Protocol == 2 {
				git.Env = append(git.Env, "GIT_PROTOCOL=version=2")
			}
			if r.Body != nil {
				defer r.Body.Close()
				git.Stdin = r.Body
			}
			data, err := git.Output()
			if err != nil {
				return nil, err
			}
			if r.Operation == GitHubGitAdvertisement && r.Protocol != 2 {
				data = append([]byte("001e# service=git-upload-pack\n0000"), data...)
			}
			if fixtureCase == "read-close" {
				return &gitVMGuestBlockedReply{ctx: ctx, active: readActive, closed: readClosed, done: make(chan struct{})}, nil
			}
			return io.NopCloser(bytes.NewReader(data)), nil
		}), func(context.Context, gitVMReadBinding) error { return nil })
		readDone <- err
	}()
	defer func() { cancel(); channels[1].Close(); <-readDone }()
	if fixtureCase == "foreign-request" {
		_ = channels[0].SetReadDeadline(time.Now().Add(8 * time.Second))
	}
	sent := q
	if fixtureCase == "foreign-request" {
		sent.Read.ChannelID = "cccccccccccccccccccccccccccccccc"
	}
	if err := writeVMRecord(channels[0], sent, gitVMRecordBytes); err != nil {
		t.Fatal(err)
	}

	waitRefusal := func() {
		t.Helper()
		select {
		case err := <-done:
			exited = true
			if err != nil {
				t.Fatal("guest failed to power off cleanly", err)
			}
		case <-time.After(8 * time.Second):
			t.Fatal("guest refusal did not stop promptly")
		}
		if (fixtureCase == "foreign-request" || fixtureCase == "control-close") && upstreamCalls.Load() != 0 {
			t.Fatal("Git work started without an exact host grant")
		}
		t.Logf("actual source guest refused %s and powered off; original outer unit still requires independent cleanup", fixtureCase)
	}
	ready, err := readVMRecord[gitVMGuestPhase](channels[0], gitVMRecordBytes)
	if fixtureCase == "foreign-request" {
		if err == nil {
			t.Fatal("foreign request reached readiness")
		}
		waitRefusal()
		return
	}

	if err != nil || !ready.valid(q, "guest-ready") {
		t.Fatalf("guest readiness: %v %+v", err, ready)
	}

	if fixtureCase == "control-close" {
		channels[0].Close()
		waitRefusal()
		return
	}
	if err := writeVMRecord(channels[0], gitVMGuestPhase{1, gitVMGuestPurpose, Digest(q), "host-grant"}, gitVMRecordBytes); err != nil {
		t.Fatal(err)
	}

	if fixtureCase == "read-close" {
		select {
		case <-readActive:
		case <-time.After(8 * time.Second):
			t.Fatal("upstream response Read never became active")
		}
		channels[1].Close()
	}
	if fixtureCase == "export-close" {
		_ = channels[2].SetReadDeadline(time.Now().Add(8 * time.Second))
		var first [1]byte
		if _, err := io.ReadFull(channels[2], first[:]); err != nil {
			t.Fatal("export never became active", err)
		}
		if upstreamCalls.Load() == 0 {
			t.Fatal("export began without Git read work")
		}
		channels[2].Close()
	}
	if fixtureCase != "normal" {
		_ = channels[0].SetReadDeadline(time.Now().Add(8 * time.Second))
		record, err := readVMRecord[gitVMGuestFinished](channels[0], gitVMRecordBytes)
		if err == nil && record.valid(q) {
			t.Fatal("lost channel reported successful acquisition")
		}

		waitRefusal()
		if fixtureCase == "read-close" {
			select {
			case <-readClosed:
			case <-time.After(2 * time.Second):
				t.Fatal("active upstream response was not closed")
			}
		}
		return
	}
	digest := sha256.New()
	measured := &gitVMGuestFixtureReader{ReadCloser: channels[2], digest: digest}
	materialized, err := receiveGitVMExport(ctx, measured, q.Source)
	if err != nil {
		t.Fatal("guest export", err)
	}
	defer materialized.Close()
	finished, err := readVMRecord[gitVMGuestFinished](channels[0], gitVMRecordBytes)
	if err != nil || !finished.valid(q) || finished.ExportBytes != measured.bytes || finished.ExportHash != hex.EncodeToString(digest.Sum(nil)) {
		t.Fatalf("guest completion: %v %+v", err, finished)
	}
	object, err := materialized.Read(ctx, snapshot.Head, "safe.go", snapshotObjectBytes)
	want, wantErr := store.Read(ctx, snapshot.Head, "safe.go", snapshotObjectBytes)
	if err != nil || wantErr != nil || !bytes.Equal(object.Data, want.Data) {
		t.Fatalf("guest source differs: %v %v", err, wantErr)
	}
	t.Logf("actual source-only guest exported %d bytes through %d broker requests; host supervisor qualification remains separate", finished.ExportBytes, finished.Broker.Requests)
}

// Host observation of the actual wire is separate from guest-reported counts.
type gitVMGuestFixtureReader struct {
	io.ReadCloser
	digest io.Writer
	bytes  int64
}

func (r *gitVMGuestFixtureReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.bytes += int64(n)
	_, _ = r.digest.Write(p[:n])
	return n, err
}

// A real accepted upstream response borrow is held in Read until disconnect.
// Close unblocks that read and records disposal; no provider/credential exists.
type gitVMGuestBlockedReply struct {
	ctx                  context.Context
	active, closed, done chan struct{}
	readOnce, closeOnce  sync.Once
}

func (r *gitVMGuestBlockedReply) Read([]byte) (int, error) {
	r.readOnce.Do(func() { close(r.active) })
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	case <-r.done:
		return 0, io.ErrClosedPipe
	}
}
func (r *gitVMGuestBlockedReply) Close() error {
	r.closeOnce.Do(func() { close(r.done); close(r.closed) })
	return nil
}
