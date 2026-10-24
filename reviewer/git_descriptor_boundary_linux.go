//go:build linux

package reviewer

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const gitDescriptorLimit = 256

// fdinfo describes the ORIGINAL descriptor's current CLOEXEC bit. A duplicate
// from pidfd_getfd always has CLOEXEC itself and cannot establish that fact.
func gitDescriptorFlags(raw []byte) (uint64, error) {
	if len(raw) == 0 || len(raw) > 16384 || raw[len(raw)-1] != '\n' || strings.ContainsAny(string(raw), "\x00\r") {
		return 0, errGitUnit
	}
	found := false
	var flags uint64
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, _ := strings.Cut(line, ":")
		if key != "flags" {
			continue
		}
		if found {
			return 0, errGitUnit
		}
		found = true
		var err error
		flags, err = strconv.ParseUint(strings.TrimSpace(value), 8, 32)
		if err != nil {
			return 0, errGitUnit
		}
	}
	if !found {
		return 0, errGitUnit
	}
	return flags, nil
}

type gitDescriptorObservation struct {
	Number                   int
	Flags                    uint64
	File                     gitKernelFileID
	Mode                     uint32
	Device                   uint64
	Target                   string
	SocketDomain, SocketType int
	SocketLocal, SocketPeer  string
	Credentials              unix.Ucred
}

// Only the retained ORIGINAL pidfd selects the task for duplication. Reading
// descriptor metadata or duplicating a descriptor never mutates its shared
// file offset, status flags, socket options or contents. Missing access refuses.
func observeGitDescriptors(ctx context.Context, identity GateIdentity, original int) (_ []gitDescriptorObservation, result error) {
	stage := "identity"
	defer func() {
		if result != nil {
			result = fmt.Errorf("Git descriptor %s: %w", stage, errGitUnit)
		}
	}()
	if ctx == nil || ctx.Err() != nil || gitCheckProcessPIDFD(identity, original) != nil {
		return nil, errGitUnit
	}
	path := fmt.Sprintf("/proc/%d", identity.PID)
	stage = "process view"
	proc, err := os.OpenRoot(path)
	if err != nil {
		return nil, errGitUnit
	}
	defer proc.Close()
	stage = "directory access"
	// fdinfo supports current CLOEXEC observation and does not require
	// traversing the nondumpable task's root-owned fd symlink directory.
	// Actual file objects still come only from the original pidfd.
	directory, err := proc.Open("fdinfo")
	if err != nil {
		return nil, errGitUnit
	}
	defer directory.Close()
	names, err := directory.Readdirnames(gitDescriptorLimit + 1)
	if err != nil && err != io.EOF || len(names) > gitDescriptorLimit {
		return nil, errGitUnit
	}
	readFlags := func(name string) (uint64, error) {
		file, err := openRootNoFollow(proc, "fdinfo/"+name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return 0, errGitUnit
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, 16385))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil {
			return 0, errGitUnit
		}
		return gitDescriptorFlags(raw)
	}
	observations := make([]gitDescriptorObservation, 0, len(names))
	for _, name := range names {
		stage = "descriptor number"
		fd, err := strconv.Atoi(name)
		if err != nil || fd < 0 || strconv.Itoa(fd) != name || ctx.Err() != nil {
			return nil, errGitUnit
		}
		stage = "original flags"
		flags, err := readFlags(name)
		if err != nil {
			return nil, errGitUnit
		}
		stage = "original duplication"
		duplicate, err := unix.PidfdGetfd(original, fd, 0)
		if err != nil {
			return nil, errGitUnit
		}
		observation, observeErr := gitObserveDescriptor(duplicate)
		target, targetErr := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", duplicate))
		closeErr := unix.Close(duplicate)
		after, afterErr := readFlags(name)
		var actual unix.Stat_t
		second, statErr := unix.PidfdGetfd(original, fd, 0)
		if statErr == nil {
			statErr = unix.Fstat(second, &actual)
			if err := unix.Close(second); statErr == nil {
				statErr = err
			}
		}
		stage = "descriptor stability"
		if observeErr != nil {
			stage = "descriptor metadata"
			return nil, errGitUnit
		}
		if closeErr != nil || afterErr != nil || flags != after {
			return nil, errGitUnit
		}
		if targetErr != nil {
			stage = "target read"
			return nil, errGitUnit
		}
		if statErr != nil {
			stage = "original stat"
			return nil, errGitUnit
		}
		if observation.File != (gitKernelFileID{uint64(actual.Dev), actual.Ino}) {
			stage = "original inode changed"
			return nil, errGitUnit
		}
		observation.Number, observation.Flags, observation.Target = fd, flags, target
		observations = append(observations, observation)
	}
	// Refuse descriptor creation/removal while checking readiness. The measured
	// inert worker must stay in its grant wait, before any candidate parser runs.
	stage = "directory stability"
	after, err := proc.Open("fdinfo")
	if err != nil {
		return nil, errGitUnit
	}
	defer after.Close()
	last, err := after.Readdirnames(gitDescriptorLimit + 1)
	if err != nil && err != io.EOF || len(last) != len(names) {
		return nil, errGitUnit
	}
	seen := map[string]bool{}
	for _, name := range names {
		seen[name] = true
	}
	for _, name := range last {
		if !seen[name] {
			return nil, errGitUnit
		}
	}
	if ctx.Err() != nil || gitCheckProcessPIDFD(identity, original) != nil {
		return nil, errGitUnit
	}
	return observations, nil
}

func gitObserveDescriptor(fd int) (gitDescriptorObservation, error) {
	var stat unix.Stat_t
	if unix.Fstat(fd, &stat) != nil || stat.Ino == 0 {
		return gitDescriptorObservation{}, errGitUnit
	}
	o := gitDescriptorObservation{File: gitKernelFileID{uint64(stat.Dev), stat.Ino}, Mode: stat.Mode, Device: stat.Rdev}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return o, nil
	}
	var err error
	o.SocketDomain, err = unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_DOMAIN)
	if err != nil {
		return o, errGitUnit
	}
	o.SocketType, err = unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TYPE)
	if err != nil {
		return o, errGitUnit
	}
	if o.SocketDomain != unix.AF_UNIX {
		// Preserve actual foreign socket evidence for policy refusal. Never
		// try to connect it or exercise its network authority.
		return o, nil
	}
	local, err := unix.Getsockname(fd)
	if err != nil {
		return o, errGitUnit
	}
	peer, err := unix.Getpeername(fd)
	if err != nil {
		return o, errGitUnit
	}
	l, ok := local.(*unix.SockaddrUnix)
	p, peerOK := peer.(*unix.SockaddrUnix)
	if !ok || !peerOK {
		return o, errGitUnit
	}
	o.SocketLocal, o.SocketPeer = l.Name, p.Name
	credentials, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil || credentials == nil {
		return o, errGitUnit
	}
	o.Credentials = *credentials
	return o, nil
}

// This is a private policy check, not reusable admission authority. Expected
// pipe/pidfd identities come from factory-owned retained kernel handles.
func validateGitDescriptors(rows []gitDescriptorObservation, export gitKernelFileID, supervisor gitPIDFDIdentity, controlPath string) error {
	if len(rows) != 8 || export == (gitKernelFileID{}) || supervisor == (gitPIDFDIdentity{}) || !filepath.IsAbs(controlPath) || filepath.Clean(controlPath) != controlPath || filepath.Base(controlPath) != "worker.sock" || len(controlPath) > 107 {
		return errGitUnit
	}
	seen := map[int]bool{}
	controlCount, exportCount, supervisorCount := 0, 0, 0
	eventCount, pollCount := 0, 0
	for _, row := range rows {
		if row.Number < 0 || seen[row.Number] {
			return errGitUnit
		}
		seen[row.Number] = true
		access := row.Flags & unix.O_ACCMODE
		if row.Number <= 2 {
			want := uint64(unix.O_WRONLY)
			if row.Number == 0 {
				want = unix.O_RDONLY
			}
			if row.Mode&unix.S_IFMT != unix.S_IFCHR || row.Device != unix.Mkdev(1, 3) || access != want && access != unix.O_RDWR {
				return errGitUnit
			}
			continue
		}
		if row.Flags&unix.O_CLOEXEC == 0 {
			return errGitUnit
		}
		switch {
		case row.Mode&unix.S_IFMT == unix.S_IFIFO && row.File == export && access == unix.O_WRONLY:
			exportCount++
		case row.Mode&unix.S_IFMT == unix.S_IFSOCK && row.SocketDomain == unix.AF_UNIX && row.SocketType == unix.SOCK_SEQPACKET && (row.SocketLocal == "" || row.SocketLocal == "@") && row.SocketPeer == controlPath && row.Credentials == (unix.Ucred{Pid: int32(os.Getpid()), Uid: uint32(os.Geteuid()), Gid: uint32(os.Getegid())}):
			controlCount++
		case row.Mode&unix.S_IFMT == 0 && row.File == gitKernelFileID(supervisor):
			supervisorCount++
		case row.Mode&unix.S_IFMT == 0 && row.Target == "anon_inode:[eventpoll]":
			pollCount++
		case row.Mode&unix.S_IFMT == 0 && row.Target == "anon_inode:[eventfd]":
			eventCount++
		default:
			return errGitUnit
		}
	}
	if !seen[0] || !seen[1] || !seen[2] || controlCount != 1 || exportCount != 1 || supervisorCount != 1 || eventCount != 1 || pollCount != 1 {
		return errGitUnit
	}
	return nil
}

func checkGitDescriptorBoundary(ctx context.Context, boundary *gitProcessBoundary, exportRead, supervisor *os.File, control *net.UnixConn) error {
	if ctx == nil || boundary == nil || exportRead == nil || supervisor == nil || control == nil {
		return errGitUnit
	}
	boundary.mu.Lock()
	defer boundary.mu.Unlock()
	if boundary.checkWorker(ctx) != nil {
		return errGitUnit
	}
	peer, err := gitBrokerPeer(control)
	if err != nil {
		return errGitUnit
	}
	defer peer.kernel.Close()
	identity, pidfd, err := retainGitBrokerPeer(peer, boundary.worker.InvocationID)
	if err != nil {
		return errGitUnit
	}
	defer unix.Close(pidfd)
	if identity != boundary.worker {
		return errGitUnit
	}
	local, ok := control.LocalAddr().(*net.UnixAddr)
	if !ok {
		return errGitUnit
	}
	pipeID, err := gitStatFileID(exportRead)
	if err != nil {
		return errGitUnit
	}
	var pipe unix.Stat_t
	flags, err := unix.FcntlInt(exportRead.Fd(), unix.F_GETFL, 0)
	if err != nil || unix.Fstat(int(exportRead.Fd()), &pipe) != nil || pipe.Mode&unix.S_IFMT != unix.S_IFIFO || flags&unix.O_ACCMODE != unix.O_RDONLY {
		return errGitUnit
	}
	want, err := gitLivePIDFDIdentity(supervisor)
	if err != nil {
		return errGitUnit
	}
	rows, err := observeGitDescriptors(ctx, boundary.worker, boundary.pidfd)
	if err != nil || validateGitDescriptors(rows, pipeID, want, local.Name) != nil || gitCheckProcessPIDFD(identity, pidfd) != nil || boundary.checkWorker(ctx) != nil {
		return errGitUnit
	}
	after, err := gitLivePIDFDIdentity(supervisor)
	if err != nil || after != want || ctx.Err() != nil {
		return errGitUnit
	}
	return nil
}
