//go:build linux

package reviewer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// GitVMGuestMain is a separate fixed guest entry point. Ordinary host execution
// refuses before opening an FD, mount, device, credential or manager connection.
func GitVMGuestMain(args []string) int {
	if len(args) == 2 && args[0] == "--source-worker" {
		return gitVMGuestWorkerMain(args[1])
	}
	if len(args) != 0 || os.Getpid() != 1 || os.Getuid() != 0 {
		return 2
	}
	defer unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
	return runGitVMGuest()
}

type gitVMGuestBrokerResult struct {
	audit GitReadBrokerAudit
	err   error
}
type gitVMGuestSetupResult struct {
	worker *gitVMGuestWorker
	err    error
}

func runGitVMGuest() int {
	stage := "setup"
	defer func() { _, _ = os.Stderr.WriteString("SFL_SOURCE_GUEST_STAGE " + stage + "\n") }()
	release, hash, err := gitVMGuestSetup()
	if err != nil {
		return 2
	}
	stage = "ports"
	control, err := gitVMGuestPort("source-control")
	if err != nil {
		return 2
	}
	defer control.Close()
	read, err := gitVMGuestPort("source-read")
	if err != nil {
		return 2
	}
	defer read.Close()
	export, err := gitVMGuestPort("source-export")
	if err != nil {
		return 2
	}
	defer export.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
	defer cancel()
	join := gitBrokerAfterFunc(ctx, func() { control.Close(); read.Close(); export.Close() })
	defer join()
	joinPorts, err := gitVMGuestWatchPorts(ctx, cancel, control, read, export)
	if err != nil {
		return 2
	}
	defer joinPorts()
	stage = "request"
	q, err := readVMRecord[gitVMGuestRequest](control, gitVMRecordBytes)
	if err != nil || !q.valid() || Digest(q) != hash || !release.matches(q) {
		return 2
	}
	client, err := newGitVMReadClient(read, q.Read)
	if err != nil {
		return 2
	}
	defer client.Close()
	if os.Mkdir(gitVMGuestBrokerPath, 0700) != nil {
		return 2
	}
	brokerCtx, brokerCancel := context.WithCancel(ctx)
	defer brokerCancel()
	setup := make(chan gitVMGuestSetupResult, 1)
	brokerDone := make(chan gitVMGuestBrokerResult, 1)
	var signalOnce sync.Once
	signal := func(w *gitVMGuestWorker, err error) { signalOnce.Do(func() { setup <- gitVMGuestSetupResult{w, err} }) }
	var admitted atomic.Pointer[gitProcessBoundary]
	go func() {
		audit, err := ServeGitReadSocket(brokerCtx, GitReadBrokerOptions{Directory: gitVMGuestBrokerPath, Source: client, Admit: func(ctx context.Context, peer GitReadPeer) error {
			boundary := admitted.Load()
			if boundary == nil {
				return ErrGitReadBroker
			}
			return boundary.checkHelper(ctx, peer)
		}}, func(GitReadEndpoint) error {
			w, err := prepareGitVMGuestWorker(brokerCtx, q)
			if err == nil {
				err = w.start(brokerCtx, q)
			}
			signal(w, err)
			return err
		})
		signal(nil, ErrVMBoot)
		brokerDone <- gitVMGuestBrokerResult{audit, err}
	}()
	// Setup always reports ownership, even if it fails while cancellation races
	// with process creation. Join it rather than abandoning an unknown child.
	stage = "worker-start"
	prepared := <-setup
	w := prepared.worker
	var stopped atomic.Bool
	defer func() {
		brokerCancel()
		client.Close()
		if w != nil && w.process != nil && !stopped.Load() {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = w.process.Stop(stopCtx)
			stopCancel()
		}
		if w != nil {
			w.closeFiles()
		}
	}()
	if prepared.err != nil || w == nil || w.process == nil {
		<-brokerDone
		return 2
	}
	stage = "worker-qualify"
	if w.qualify(ctx, q) != nil {
		brokerCancel()
		<-brokerDone
		return 2
	}
	stage = "host-grant"
	admitted.Store(w.boundary)
	if writeVMRecord(control, gitVMGuestPhase{1, gitVMGuestPurpose, hash, "guest-ready"}, gitVMRecordBytes) != nil {
		brokerCancel()
		<-brokerDone
		return 2
	}
	grant, err := readVMRecord[gitVMGuestPhase](control, gitVMRecordBytes)
	if err != nil || !grant.valid(q, "host-grant") {
		brokerCancel()
		<-brokerDone
		return 2
	}
	stage = "export"
	return finishGitVMGuest(ctx, cancel, q, w, control, export, client, brokerCancel, brokerDone, &stopped)
}

func finishGitVMGuest(ctx context.Context, cancel context.CancelFunc, q gitVMGuestRequest, w *gitVMGuestWorker, control, export *os.File, client *gitVMReadClient, brokerCancel context.CancelFunc, brokerDone <-chan gitVMGuestBrokerResult, stopped *atomic.Bool) int {
	// After the one host grant, any further input or peer closure cancels work.
	// The original host keeps this channel open until the finished record.
	controlDone := make(chan struct{})
	go func() { defer close(controlDone); var extra [1]byte; _, _ = control.Read(extra[:]); cancel() }()
	defer func() { control.Close(); <-controlDone }()
	raw, _ := json.Marshal(gitVMGuestPhase{1, gitVMGuestPurpose, Digest(q), "worker-grant"})
	if writeGitControlPacket(ctx, w.control, raw, nil) != nil {
		brokerCancel()
		<-brokerDone
		return 2
	}
	digest := sha256.New()
	writer := &gitVMGuestExportWriter{ctx: ctx, target: io.MultiWriter(export, digest), remaining: gitExportWireBytes + gitVMRecordBytes + 4}
	copyDone := make(chan error, 1)
	stopCopy := gitBrokerAfterFunc(ctx, func() { w.process.out.Close(); export.Close() })
	defer stopCopy()
	go func() { _, err := io.CopyBuffer(writer, w.process.out, make([]byte, 32<<10)); copyDone <- err }()
	copyErr := <-copyDone
	if copyErr != nil {
		cancel()
	}
	// Pipe EOF precedes the trusted worker's final return. Observe its natural
	// exit before stopping escaped descendants in the original cgroup.
	select {
	case <-w.process.done:
	case <-ctx.Done():
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	stopErr := w.process.Stop(stopCtx)
	stopCancel()
	stopped.Store(stopErr == nil)
	brokerCancel()
	client.Close()
	broker := <-brokerDone
	if copyErr != nil || stopErr != nil || w.process.waitErr != nil || ctx.Err() != nil || broker.audit.Failed || !broker.audit.Stopped {
		return 2
	}
	finished := gitVMGuestFinished{1, gitVMGuestPurpose, Digest(q), writer.bytes, hex.EncodeToString(digest.Sum(nil)), broker.audit, true}
	if !finished.valid(q) || writeVMRecord(control, finished, gitVMRecordBytes) != nil {
		return 2
	}
	return 0
}

type gitVMGuestExportWriter struct {
	ctx              context.Context
	target           io.Writer
	remaining, bytes int64
}

func (w *gitVMGuestExportWriter) Write(raw []byte) (int, error) {
	if w.ctx.Err() != nil || int64(len(raw)) > w.remaining {
		return 0, ErrVMBoot
	}
	n, err := w.target.Write(raw)
	w.bytes += int64(n)
	w.remaining -= int64(n)
	if err != nil {
		return n, err
	}
	if n != len(raw) {
		return n, io.ErrShortWrite
	}
	return n, nil
}
