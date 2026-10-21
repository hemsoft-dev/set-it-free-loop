//go:build linux

package reviewer

import (
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

type gitNamespaceIdentity struct{ Device, Inode uint64 }
type gitNetworkBoundary struct {
	mu                          sync.Mutex
	worker, manager             GateIdentity
	workerPIDFD, managerPIDFD   int
	hostNS, managerNS, workerNS *os.File
	closed                      bool
}

func gitNetworkNamespace(pid int) (*os.File, gitNamespaceIdentity, error) {
	if pid <= 0 {
		return nil, gitNamespaceIdentity{}, errGitUnit
	}
	fd, err := unix.Open("/proc/"+strconv.Itoa(pid)+"/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, gitNamespaceIdentity{}, errGitUnit
	}
	file := os.NewFile(uintptr(fd), "sfl-git-network-namespace")
	identity, err := gitNetworkNamespaceIdentity(file)
	if err != nil {
		file.Close()
		return nil, gitNamespaceIdentity{}, err
	}
	return file, identity, nil
}
func gitNetworkNamespaceIdentity(file *os.File) (gitNamespaceIdentity, error) {
	if file == nil {
		return gitNamespaceIdentity{}, errGitUnit
	}
	kind, err := unix.IoctlRetInt(int(file.Fd()), unix.NS_GET_NSTYPE)
	var stat unix.Stat_t
	if err != nil || kind != unix.CLONE_NEWNET || unix.Fstat(int(file.Fd()), &stat) != nil || stat.Ino == 0 {
		return gitNamespaceIdentity{}, errGitUnit
	}
	return gitNamespaceIdentity{uint64(stat.Dev), stat.Ino}, nil
}

// Duplicate first: a caller closing its fd cannot turn our later observations
// into observations of an unrelated descriptor reused at the same number.
func gitRetainProcessPIDFD(identity GateIdentity, original int) (int, error) {
	if original < 0 {
		return -1, errGitUnit
	}
	fd, err := unix.FcntlInt(uintptr(original), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return -1, errGitUnit
	}
	if gitCheckProcessPIDFD(identity, fd) != nil {
		unix.Close(fd)
		return -1, errGitUnit
	}
	return fd, nil
}
func gitCheckProcessPIDFD(identity GateIdentity, fd int) error {
	if fd < 0 || identity.PID <= 0 || unix.PidfdSendSignal(fd, 0, nil, 0) != nil {
		return errGitUnit
	}
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	if n, err := unix.Poll(poll, 0); err != nil || n != 0 || poll[0].Revents != 0 {
		return errGitUnit
	}
	// A live pidfd alone is insufficient: prove it names the expected process.
	info, err := os.Open("/proc/self/fdinfo/" + strconv.Itoa(fd))
	if err != nil {
		return errGitUnit
	}
	raw, readErr := io.ReadAll(io.LimitReader(info, 4097))
	info.Close()
	if readErr != nil || len(raw) > 4096 {
		return errGitUnit
	}
	pid, count := 0, 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "Pid:") {
			count++
			pid, err = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Pid:")))
			if err != nil {
				return errGitUnit
			}
		}
	}
	actual, err := processGateIdentity(identity.PID, identity.InvocationID)
	poll[0].Revents = 0
	n, pollErr := unix.Poll(poll, 0)
	if count != 1 || pid != identity.PID || err != nil || actual != identity || unix.PidfdSendSignal(fd, 0, nil, 0) != nil || pollErr != nil || n != 0 || poll[0].Revents != 0 {
		return errGitUnit
	}
	return nil
}

// Factory-only kernel observation. The future factory must first authenticate
// the inert inner worker and manager identities against the owning unit/bus.
// This helper proves only the network namespace boundary, never runtime,
// cgroup, ancestry, syscall filtering, route absence or permission to fetch.
func retainGitNetworkBoundary(worker GateIdentity, workerPIDFD int, manager GateIdentity, managerPIDFD int) (*gitNetworkBoundary, error) {
	b := &gitNetworkBoundary{worker: worker, manager: manager, workerPIDFD: -1, managerPIDFD: -1}
	good := false
	defer func() {
		if !good {
			b.Close()
		}
	}()
	var err error
	if b.workerPIDFD, err = gitRetainProcessPIDFD(worker, workerPIDFD); err != nil {
		return nil, err
	}
	if b.managerPIDFD, err = gitRetainProcessPIDFD(manager, managerPIDFD); err != nil {
		return nil, err
	}
	var host, managing, isolated gitNamespaceIdentity
	if b.hostNS, host, err = gitNetworkNamespace(os.Getpid()); err != nil {
		return nil, err
	}
	if b.managerNS, managing, err = gitNetworkNamespace(manager.PID); err != nil {
		return nil, err
	}
	if b.workerNS, isolated, err = gitNetworkNamespace(worker.PID); err != nil {
		return nil, err
	}
	if isolated == host || isolated == managing || b.recheckLocked() != nil {
		return nil, errGitUnit
	}
	good = true
	return b, nil
}
func gitMatchNetworkNamespace(pid int, expected *os.File) error {
	file, current, err := gitNetworkNamespace(pid)
	if err != nil {
		return err
	}
	defer file.Close()
	want, err := gitNetworkNamespaceIdentity(expected)
	if err != nil || current != want {
		return errGitUnit
	}
	return nil
}
func (b *gitNetworkBoundary) recheckLocked() error {
	if b.closed || gitCheckProcessPIDFD(b.worker, b.workerPIDFD) != nil || gitCheckProcessPIDFD(b.manager, b.managerPIDFD) != nil ||
		gitMatchNetworkNamespace(os.Getpid(), b.hostNS) != nil || gitMatchNetworkNamespace(b.manager.PID, b.managerNS) != nil || gitMatchNetworkNamespace(b.worker.PID, b.workerNS) != nil ||
		gitCheckProcessPIDFD(b.worker, b.workerPIDFD) != nil || gitCheckProcessPIDFD(b.manager, b.managerPIDFD) != nil {
		return errGitUnit
	}
	return nil
}
func (b *gitNetworkBoundary) recheck() error {
	if b == nil {
		return errGitUnit
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.recheckLocked()
}

// peer is supplied only from the broker's original kernel credential/pidfd
// observation. The factory still checks the measured helper, unit and ancestry.
func (b *gitNetworkBoundary) matchesPeer(peer GateIdentity, peerPIDFD int) error {
	if b == nil {
		return errGitUnit
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.recheckLocked() != nil {
		return errGitUnit
	}
	fd, err := gitRetainProcessPIDFD(peer, peerPIDFD)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if gitMatchNetworkNamespace(peer.PID, b.workerNS) != nil || gitCheckProcessPIDFD(peer, fd) != nil || b.recheckLocked() != nil {
		return errGitUnit
	}
	return nil
}
func (b *gitNetworkBoundary) Close() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	for _, fd := range []int{b.workerPIDFD, b.managerPIDFD} {
		if fd >= 0 {
			unix.Close(fd)
		}
	}
	for _, file := range []*os.File{b.hostNS, b.managerNS, b.workerNS} {
		if file != nil {
			file.Close()
		}
	}
	return nil
}
