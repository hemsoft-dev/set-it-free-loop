//go:build linux

package reviewer

import (
	"bytes"
	"encoding/binary"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// This is the fixed late bwrap filter, installed after namespace/mount setup
// and before exec. It is not a sandbox qualification or an admission grant.
// The measured bootstrap may create NETLINK_ROUTE sockets to initialize its
// private network namespace; no Git subprocess inherits that permission.
func gitPayloadSeccomp() ([]unix.SockFilter, error) {
	if runtime.GOARCH != "amd64" {
		return nil, errGitUnit
	}
	load := func(offset uint32) unix.SockFilter {
		return unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: offset}
	}
	jeq := func(value uint32, yes, no uint8) unix.SockFilter {
		return unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: value, Jt: yes, Jf: no}
	}
	ret := func(value uint32) unix.SockFilter { return unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: value} }
	denied := uint32(unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM))
	filter := []unix.SockFilter{
		load(4), jeq(unix.AUDIT_ARCH_X86_64, 1, 0), ret(unix.SECCOMP_RET_KILL_PROCESS),
		load(0),
		{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, K: 0x40000000, Jf: 1},
		ret(unix.SECCOMP_RET_KILL_PROCESS), // Reject the x32 ABI too.
	}
	// clone3 hides namespace flags behind a pointer. ENOSYS permits libc's
	// ordinary-thread fallback to clone, whose flags can be checked directly.
	filter = append(filter, jeq(435, 0, 1), ret(unix.SECCOMP_RET_ERRNO|uint32(unix.ENOSYS)))
	for _, call := range []uint32{
		101, 310, 311, 312, // ptrace, process_vm_readv/writev, kcmp
		424, 434, 438, 440, 448, // pidfd signaling/open/getfd, process_madvise/mrelease
		425, 426, 427, // All io_uring APIs, including use of an inherited ring.
		155, 161, 165, 166, 272, 308, // pivot_root, chroot, mount, umount2, unshare, setns
		428, 429, 430, 431, 432, 433, 442, // New mount APIs.
		248, 249, 250, 298, 300, 303, 304, 321, 323, // Keyrings, perf, fanotify, handles, BPF, userfaultfd.
	} {
		filter = append(filter, jeq(call, 0, 1), ret(denied))
	}
	filter = append(filter,
		jeq(56, 0, 4), load(16), // clone's low flag word
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, K: 0x7e020000, Jf: 1},
		ret(denied), load(0),
		jeq(41, 2, 0), jeq(53, 1, 0), ret(unix.SECCOMP_RET_ALLOW), // socket/socketpair
		load(16), jeq(unix.AF_UNIX, 1, 0),
		ret(unix.SECCOMP_RET_ERRNO|uint32(unix.EAFNOSUPPORT)), ret(unix.SECCOMP_RET_ALLOW),
	)
	return filter, nil
}

// A sealed, non-executable, close-on-exec input. The bootstrap must explicitly
// pass only this descriptor to the measured bwrap --seccomp argument. Bwrap
// consumes it before payload exec; no host directory FD is supplied here.
func retainGitPayloadSeccomp() (*os.File, error) {
	filter, err := gitPayloadSeccomp()
	if err != nil {
		return nil, err
	}
	var data bytes.Buffer
	if binary.Write(&data, binary.LittleEndian, filter) != nil || data.Len() > 4096 {
		return nil, errGitUnit
	}
	fd, err := unix.MemfdCreate("sfl-git-payload-filter", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING|unix.MFD_NOEXEC_SEAL)
	if err != nil {
		return nil, errGitUnit
	}
	file := os.NewFile(uintptr(fd), "sfl-git-payload-filter")
	good := false
	defer func() {
		if !good {
			file.Close()
		}
	}()
	if _, err := file.Write(data.Bytes()); err != nil || file.Chmod(0400) != nil {
		return nil, errGitUnit
	}
	if _, err := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, requiredStubSeals); err != nil {
		return nil, errGitUnit
	}
	if _, err := file.Seek(0, 0); err != nil {
		return nil, errGitUnit
	}
	good = true
	return file, nil
}
