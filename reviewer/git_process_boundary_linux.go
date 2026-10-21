//go:build linux

package reviewer

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"sync"

	"golang.org/x/sys/unix"
)

// This private boundary proves only process/runtime ancestry policy. The factory
// must independently bind the original worker to its sealed grant, exact unit,
// cgroup, mounts, descriptors, routes and installed filter. No constructor here
// grants a Git capability or accepts caller-supplied observation booleans.
type gitProcessBoundary struct {
	mu                     sync.Mutex
	worker                 GateIdentity
	pidfd                  int
	root, image, git, http *os.File
	workerPin              VMAssetPin
	pins                   GitRuntimePins
	namespaces             map[string]gitNamespaceIdentity
	cgroup                 string
	maps                   map[gitKernelFileID]bool
	closed                 bool
}

func retainGitProcessBoundary(ctx context.Context, worker GateIdentity, original int, root, image *os.File, workerPin VMAssetPin, pins GitRuntimePins) (_ *gitProcessBoundary, result error) {
	b := &gitProcessBoundary{worker: worker, pidfd: -1, workerPin: workerPin, pins: pins, maps: map[gitKernelFileID]bool{}}
	defer func() {
		if result != nil {
			b.Close()
		}
	}()
	if ctx == nil || ctx.Err() != nil {
		return nil, errGitUnit
	}
	var err error
	if b.pidfd, err = gitRetainProcessPIDFD(worker, original); err != nil {
		return nil, errGitUnit
	}
	if b.root, err = duplicateGitControlFile(root); err != nil {
		return nil, errGitUnit
	}
	if b.image, err = duplicateGitControlFile(image); err != nil {
		return nil, errGitUnit
	}
	m, err := verifyGitRuntimeDirectory(ctx, b.root, pins)
	if err != nil {
		return nil, errGitUnit
	}
	view, err := os.OpenRoot(fmt.Sprintf("/proc/self/fd/%d", b.root.Fd()))
	if err != nil {
		return nil, errGitUnit
	}
	defer view.Close()
	for _, pin := range m.Files {
		file, err := view.OpenFile(pin.Path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return nil, errGitUnit
		}
		id, err := gitStatFileID(file)
		if err != nil {
			file.Close()
			return nil, errGitUnit
		}
		b.maps[id] = true
		switch pin.Path {
		case gitRuntimeProgram:
			b.git = file
		case gitRuntimeHTTP:
			b.http = file
		default:
			file.Close()
		}
	}
	observation, err := observeGitProcess(ctx, worker, b.pidfd, b.root, b.image, workerPin)
	if err != nil {
		return nil, errGitUnit
	}
	for name, id := range observation.Namespaces {
		file, err := os.Open("/proc/self/ns/" + name)
		if err != nil {
			return nil, errGitUnit
		}
		host, statErr := gitStatFileID(file)
		file.Close()
		if statErr != nil || id == gitNamespaceIdentity(host) {
			return nil, errGitUnit
		}
	}
	b.namespaces = observation.Namespaces
	b.cgroup = observation.Cgroup
	if b.checkPolicy(observation) != nil || b.checkWorker(ctx) != nil {
		return nil, errGitUnit
	}
	return b, nil
}

func (b *gitProcessBoundary) checkPolicy(observation gitProcessObservation) error {
	if observation.RootFlags&unix.ST_RDONLY == 0 || observation.NoNewPrivileges != 1 || observation.Seccomp != 2 || len(observation.Capabilities) != 5 || len(observation.Namespaces) != 6 || !reflect.DeepEqual(observation.Namespaces, b.namespaces) || observation.Cgroup != b.cgroup || b.cgroup == "" {
		return errGitUnit
	}
	for _, id := range observation.UIDs {
		if id != uint32(os.Geteuid()) {
			return errGitUnit
		}
	}
	for _, id := range observation.GIDs {
		if id != uint32(os.Getegid()) {
			return errGitUnit
		}
	}
	for _, value := range observation.Capabilities {
		if value != 0 {
			return errGitUnit
		}
	}
	if len(observation.Network.Interfaces) != 1 || observation.Network.Interfaces[0] != "lo" || observation.Network.IPv4Routes != 0 || observation.Network.IPv6Routes != 0 || observation.Network.IPv6Rejects > 2 || observation.Network.IPv6Loopback > 1 {
		return errGitUnit
	}
	own := false
	for _, mapping := range observation.ExecutableMaps {
		if mapping.Writable {
			return errGitUnit
		}
		if mapping.File == observation.Executable {
			own = true
			if observation.Identity == b.worker {
				continue
			}
		}
		if b.maps[mapping.File] {
			continue
		}
		if mapping.File == (gitKernelFileID{}) && (mapping.KernelName == "[vdso]" || mapping.KernelName == "[vsyscall]") {
			continue
		}
		return errGitUnit
	}
	if !own {
		return errGitUnit
	}
	return nil
}
func (b *gitProcessBoundary) checkWorker(ctx context.Context) error {
	if b.closed {
		return errGitUnit
	}
	observation, err := observeGitProcess(ctx, b.worker, b.pidfd, b.root, b.image, b.workerPin)
	if err != nil || b.checkPolicy(observation) != nil {
		return errGitUnit
	}
	return nil
}

// Retain the actual SO_PEERPIDFD and every live intermediate ancestor until all
// edges are re-observed. Only measured Git ancestors may lead from the exact
// HTTP helper to the original worker. A metadata-only peer cannot enter here.
func (b *gitProcessBoundary) checkHelper(ctx context.Context, peer GitReadPeer) error {
	if b == nil || ctx == nil {
		return errGitUnit
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if ctx.Err() != nil || peer.UID != uint32(os.Geteuid()) || peer.GID != uint32(os.Getegid()) || b.checkWorker(ctx) != nil {
		return errGitUnit
	}
	identity, fd, err := retainGitBrokerPeer(peer, b.worker.InvocationID)
	if err != nil {
		return errGitUnit
	}
	type link struct {
		identity GateIdentity
		fd       int
		file     *os.File
		pin      VMAssetPin
		parent   int
	}
	chain := []link{{identity: identity, fd: fd, file: b.http, pin: b.pins.HTTP}}
	defer func() {
		for _, item := range chain {
			unix.Close(item.fd)
		}
	}()
	reached := false
	for len(chain) <= 8 {
		index := len(chain) - 1
		item := &chain[index]
		observation, err := observeGitProcess(ctx, item.identity, item.fd, b.root, item.file, item.pin)
		if err != nil || b.checkPolicy(observation) != nil {
			return errGitUnit
		}
		item.parent = observation.ParentPID
		if item.parent == b.worker.PID {
			// The actual fetch has at least one Git parent. A helper launched
			// directly by an unrelated sibling cannot skip that edge.
			if len(chain) < 2 {
				return errGitUnit
			}
			reached = true
			break
		}
		if item.parent <= 1 || item.parent == item.identity.PID || len(chain) == 8 {
			return errGitUnit
		}
		for _, prior := range chain {
			if prior.identity.PID == item.parent {
				return errGitUnit
			}
		}
		parent, err := processGateIdentity(item.parent, b.worker.InvocationID)
		if err != nil || parent.BootID != b.worker.BootID {
			return errGitUnit
		}
		parentFD, err := unix.PidfdOpen(parent.PID, 0)
		if err != nil {
			return errGitUnit
		}
		chain = append(chain, link{identity: parent, fd: parentFD, file: b.git, pin: b.pins.Git})
	}
	if !reached {
		return errGitUnit
	}
	for index := len(chain) - 1; index >= 0; index-- {
		item := chain[index]
		observation, err := observeGitProcess(ctx, item.identity, item.fd, b.root, item.file, item.pin)
		if err != nil || observation.ParentPID != item.parent || b.checkPolicy(observation) != nil {
			return errGitUnit
		}
	}
	if ctx.Err() != nil || b.checkWorker(ctx) != nil {
		return errGitUnit
	}
	return nil
}
func (b *gitProcessBoundary) Close() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	if b.pidfd >= 0 {
		unix.Close(b.pidfd)
	}
	return closeVMFiles([]*os.File{b.root, b.image, b.git, b.http})
}
