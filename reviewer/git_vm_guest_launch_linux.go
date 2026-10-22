//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

type gitVMGuestWorker struct {
	process                                           *nativeGuestProcess // original-cgroup process primitive, no auth state
	image, request, runtime, broker, work, supervisor *os.File
	listener                                          *net.UnixListener
	boundary                                          *gitProcessBoundary
	control                                           *net.UnixConn
}

func gitVMGuestWorkerArgs(q gitVMGuestRequest) ([]string, error) {
	if !q.valid() {
		return nil, ErrVMBoot
	}
	return []string{"--unshare-all", "--as-pid-1", "--unshare-user", "--disable-userns", "--die-with-parent", "--new-session", "--uid", "1203", "--gid", "1203", "--cap-drop", "ALL", "--clearenv",
		"--ro-bind-fd", "8", "/", "--ro-bind-fd", "9", "/broker", "--bind-fd", "10", "/work", "--bind-fd", "11", "/tmp",
		"--proc", "/proc", "--remount-ro", "/proc", "--dev-bind", "/dev/null", "/dev/null", "--dev-bind", "/dev/urandom", "/dev/urandom", "--remount-ro", "/dev/urandom",
		"--chdir", "/work", "--seccomp", "7", "--argv0", "sfl-source-worker", "--", "/proc/self/fd/4", "--source-worker", Digest(q)}, nil
}

// Called only by the trusted source guest's PID1, after the fixed Git broker
// listener exists. Host paths and arbitrary commands cannot enter this role.
func prepareGitVMGuestWorker(ctx context.Context, q gitVMGuestRequest) (_ *gitVMGuestWorker, result error) {
	if os.Getpid() != 1 || os.Getuid() != 0 || ctx == nil || ctx.Err() != nil || !q.valid() {
		return nil, ErrVMBoot
	}
	w := &gitVMGuestWorker{}
	defer func() {
		if result != nil {
			w.closeFiles()
		}
	}()
	data, err := readGitVMGuestFile(gitVMGuestWorkerPath, 64<<20)
	if err != nil || int64(len(data)) != q.Source.Manifest.Worker.Bytes || gitRawHash(data) != q.Source.Manifest.Worker.SHA256 {
		return nil, ErrVMBoot
	}
	w.image, err = gitVMGuestSeal(ctx, data, true)
	clear(data)
	if err != nil {
		return nil, ErrVMBoot
	}
	raw, err := json.Marshal(q)
	if err != nil {
		return nil, ErrVMBoot
	}
	w.request, err = gitVMGuestSeal(ctx, raw, false)
	if err != nil {
		return nil, ErrVMBoot
	}
	if os.Mkdir("/run/source-runtime", 0700) != nil {
		return nil, ErrVMBoot
	}
	artifact, err := os.OpenFile(gitVMGuestRuntimePath, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrVMBoot
	}
	prepared, err := prepareGitRuntime(ctx, "/run/source-runtime", artifact, q.Source.Manifest.Runtime, nil)
	artifact.Close()
	if err != nil {
		return nil, ErrVMBoot
	}
	// Runtime bytes are immutable until this whole ephemeral VM is destroyed.
	// Its directory descriptor, not its original pathname, enters bwrap.
	w.runtime, err = prepared.root.Open(".")
	// Close management descriptors on both success and failure. The whole
	// ephemeral VM owns the immutable runtime files until poweroff.
	prepared.root.Close()
	prepared.parent.Close()
	if err != nil {
		return nil, ErrVMBoot
	}
	// bwrap resolves bind-fd source paths before mounting. Permit traversal
	// of this root-owned guest-only parent, without granting write access.
	if os.Chmod("/run/source-runtime", 0711) != nil {
		return nil, ErrVMBoot
	}
	w.work, err = openGitTmpfsWorkspace(q.Source.Binding)
	if err != nil || unix.Fchown(int(w.work.Fd()), gitVMGuestUID, gitVMGuestGID) != nil {
		return nil, ErrVMBoot
	}
	// The unprivileged namespace helper needs its private outer /tmp during
	// pivot setup. Only the worker owns this guest-only filesystem; the payload
	// later sees the captured work inode at both /work and /tmp.
	if os.Chown("/tmp", gitVMGuestUID, gitVMGuestGID) != nil {
		return nil, ErrVMBoot
	}
	rootPIDFD, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		return nil, ErrVMBoot
	}
	unix.CloseOnExec(rootPIDFD)
	w.supervisor = os.NewFile(uintptr(rootPIDFD), "source-root-pidfd")
	w.listener, err = net.ListenUnix("unixpacket", &net.UnixAddr{Name: gitVMGuestControlPath, Net: "unixpacket"})
	if err != nil {
		return nil, ErrVMBoot
	}
	w.listener.SetUnlinkOnClose(false)
	for _, name := range []string{gitVMGuestControlPath, gitVMGuestBrokerPath + "/git.sock"} {
		if os.Chmod(name, 0600) != nil || os.Chown(name, gitVMGuestUID, gitVMGuestGID) != nil {
			return nil, ErrVMBoot
		}
	}
	if os.Chmod(gitVMGuestBrokerPath, 0555) != nil {
		return nil, ErrVMBoot
	}
	w.broker, err = os.Open(gitVMGuestBrokerPath)
	if err != nil || verifyGitBrokerDirectoryIDs(w.broker, 0, gitVMGuestUID, 0555) != nil {
		return nil, ErrVMBoot
	}
	return w, nil
}

func (w *gitVMGuestWorker) start(ctx context.Context, q gitVMGuestRequest) error {
	if os.Getpid() != 1 || os.Getuid() != 0 || w == nil || w.process != nil || ctx == nil || ctx.Err() != nil {
		return ErrVMBoot
	}
	args, err := gitVMGuestWorkerArgs(q)
	if err != nil {
		return err
	}
	filter, err := gitPayloadSeccomp()
	if err != nil {
		return ErrVMBoot
	}
	var wire bytes.Buffer
	if binary.Write(&wire, binary.LittleEndian, filter) != nil || gitRawHash(wire.Bytes()) != q.Source.Manifest.Filter.SHA256 {
		return ErrVMBoot
	}
	sealedFilter, err := gitVMGuestSeal(ctx, wire.Bytes(), false)
	if err != nil {
		return ErrVMBoot
	}
	defer sealedFilter.Close()
	alias, err := duplicateGitControlFile(w.work)
	if err != nil {
		return ErrVMBoot
	}
	defer alias.Close()
	group := "/sys/fs/cgroup/source-worker"
	if os.Mkdir(group, 0700) != nil {
		return ErrVMBoot
	}
	for key, value := range map[string]string{"memory.max": "402653184", "memory.swap.max": "0", "pids.max": "64", "cpu.max": "100000 100000"} {
		if os.WriteFile(group+"/"+key, []byte(value), 0600) != nil {
			return ErrVMBoot
		}
		actual, err := os.ReadFile(group + "/" + key)
		if err != nil || string(bytes.TrimSpace(actual)) != value {
			return ErrVMBoot
		}
	}
	groupFD, err := os.Open(group)
	if err != nil {
		return ErrVMBoot
	}
	read, write, err := os.Pipe()
	if err != nil {
		groupFD.Close()
		return ErrVMBoot
	}
	devNull, err := os.OpenFile("/dev/null", os.O_RDWR, 0)
	if err != nil {
		groupFD.Close()
		read.Close()
		write.Close()
		return ErrVMBoot
	}
	defer devNull.Close()
	cmd := exec.Command(gitVMGuestHelperPath, args...)
	cmd.Env = []string{}
	cmd.Dir = "/"
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = &gitVMGuestDiagnostic{}
	cmd.ExtraFiles = []*os.File{w.supervisor, w.image, w.request, write, sealedFilter, w.runtime, w.broker, w.work, alias}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: gitVMGuestUID, Gid: gitVMGuestGID, Groups: []uint32{}}, UseCgroupFD: true, CgroupFD: int(groupFD.Fd()), Pdeathsig: syscall.SIGKILL}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != nil {
		groupFD.Close()
		read.Close()
		write.Close()
		return ErrVMBoot
	}
	if err := cmd.Start(); err != nil {
		groupFD.Close()
		read.Close()
		write.Close()
		return ErrVMBoot
	}
	write.Close()
	w.process = &nativeGuestProcess{command: cmd, group: group, groupFD: groupFD, out: read, done: make(chan struct{})}
	go func() { w.process.waitErr = cmd.Wait(); close(w.process.done) }()
	return nil
}

func (w *gitVMGuestWorker) qualify(ctx context.Context, q gitVMGuestRequest) error {
	if w == nil || w.process == nil || ctx == nil || ctx.Err() != nil {
		return ErrVMBoot
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-w.process.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	join := gitBrokerAfterFunc(ctx, func() { w.listener.Close() })
	defer join()
	conn, err := w.listener.AcceptUnix()
	if err != nil {
		return ErrVMBoot
	}
	w.control = conn
	pid, uid, fd, err := gatePeer(conn)
	if fd >= 0 {
		defer unix.Close(fd)
	}
	if err != nil || uid != gitVMGuestUID {
		return ErrVMBoot
	}
	// The all-zero manager coordinate is a private kernel-helper placeholder,
	// as in gitBrokerPeerCapability. No guest systemd invocation is invented or
	// serialized, and this cannot qualify a host manager or provider auth lease.
	identity, err := processGateIdentity(pid, "00000000000000000000000000000000")
	if err != nil {
		return ErrVMBoot
	}
	raw, files, err := readGitControlPacket(ctx, conn, 0)
	closeVMFiles(files)
	ready, decodeErr := DecodeStrict[gitVMGuestPhase](raw, gitVMRecordBytes)
	canonical, _ := json.Marshal(ready)
	if err != nil || decodeErr != nil || !bytes.Equal(raw, canonical) || !ready.valid(q, "worker-ready") {
		return ErrVMBoot
	}
	w.boundary, err = retainGitProcessBoundaryIDs(ctx, identity, fd, w.runtime, w.image, q.Source.Manifest.Worker, q.Source.Manifest.Runtime, gitVMGuestUID, gitVMGuestGID)
	if err != nil || w.boundary.cgroup != "/source-worker" || checkGitMountBoundaryIDs(ctx, w.boundary, w.broker, q.Source.Binding, 0, 0555) != nil ||
		checkGitDescriptorBoundary(ctx, w.boundary, w.process.out, w.supervisor, conn) != nil {
		return ErrVMBoot
	}
	// The accepted process must be PID1 in its private namespace, never a
	// sibling or a restarted trusted binary in the same acquisition cgroup.
	status, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil || !gitVMGuestPID1(status) {
		return ErrVMBoot
	}
	return nil
}

func gitVMGuestPID1(status []byte) bool {
	for _, line := range bytes.Split(status, []byte{'\n'}) {
		fields := bytes.Fields(line)
		if len(fields) > 1 && string(fields[0]) == "NSpid:" {
			return len(fields) == 3 && string(fields[2]) == "1"
		}
	}
	return false
}

func (w *gitVMGuestWorker) closeFiles() {
	if w == nil {
		return
	}
	if w.listener != nil {
		w.listener.Close()
	}
	if w.control != nil {
		w.control.Close()
	}
	if w.boundary != nil {
		w.boundary.Close()
	}
	if w.process != nil && w.process.out != nil {
		w.process.out.Close()
	}
	_ = closeVMFiles([]*os.File{w.image, w.request, w.runtime, w.broker, w.work, w.supervisor})
}

// Child output never passes through to serial. Only a fixed trusted stage
// record is emitted; unknown helper/Git output becomes one constant marker.
type gitVMGuestDiagnostic struct {
	raw     []byte
	emitted bool
}

func (d *gitVMGuestDiagnostic) Write(raw []byte) (int, error) {
	n := len(raw)
	if d.emitted {
		return n, nil
	}
	if len(d.raw)+len(raw) > 128 {
		d.emitted = true
		_, _ = os.Stderr.WriteString("SFL_SOURCE_WORKER_STAGE helper-error\n")
		return n, nil
	}
	d.raw = append(d.raw, raw...)
	if bytes.Contains(d.raw, []byte{'\n'}) {
		d.emitted = true
		for _, stage := range []string{"inherited", "manifest", "connect", "authenticate", "policy", "ready", "acquire"} {
			if string(d.raw) == "SFL_SOURCE_WORKER_STAGE "+stage+"\n" {
				_, _ = os.Stderr.Write(d.raw)
				return n, nil
			}
		}

		kind := "helper-error"
		text := string(d.raw)
		for _, item := range []struct{ needle, kind string }{
			{"new namespace", "helper-namespace"}, {"uid map", "helper-uid-map"},
			{"mount", "helper-mount"}, {"execvp", "helper-exec"},
			{"Permission denied", "helper-permission"}, {"Operation not permitted", "helper-permission"},
		} {
			if strings.Contains(text, item.needle) {
				kind = item.kind
				break
			}
		}
		_, _ = os.Stderr.WriteString("SFL_SOURCE_WORKER_STAGE " + kind + "\n")
		// Temporary credential-free image diagnostic; remove before PR.
		_, _ = os.Stderr.WriteString("SFL_SOURCE_HELPER_DIAGNOSTIC " + strconv.QuoteToASCII(text) + "\n")
	}
	return n, nil
}
