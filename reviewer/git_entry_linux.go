//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

func jsonMarshalGitBinding(binding gitMaterializationBinding) (string, error) {
	if !binding.valid() {
		return "", errGitUnit
	}
	raw, err := json.Marshal(binding)
	return string(raw), err
}

func parseGitProcessBinding(raw, hash string) (gitMaterializationBinding, error) {
	zero := gitMaterializationBinding{}
	binding, err := DecodeStrict[gitMaterializationBinding]([]byte(raw), 4096)
	canonical, marshalErr := json.Marshal(binding)
	if err != nil || marshalErr != nil || !bytes.Equal(canonical, []byte(raw)) || !binding.valid() || Digest(binding) != hash {
		return zero, errGitUnit
	}
	return binding, nil
}

// GitAcquisitionProcess is solely the fixed executable entry point. It takes
// ownership of its fixed inherited FD slots. Never call it in the controller
// process. There is no credential, model, candidate command or URL argument.
// Starting this binary does not qualify a unit or grant any source authority.
func GitAcquisitionProcess(ctx context.Context, args []string, invocation string) error {
	if ctx == nil || ctx.Err() != nil || !gitQuarantineHex(invocation, 32) {
		return errGitUnit
	}
	bounded, cancel := context.WithTimeout(ctx, 110*time.Second)
	defer cancel()
	if len(args) == 3 && args[0] == "--worker" {
		binding, err := parseGitProcessBinding(args[1], args[2])
		if err != nil {
			return errGitUnit
		}
		if gitProcessInheritedRoles(true) != nil {
			return errGitUnit
		}
		return runGitAcquisitionWorker(bounded, gitWorkerInput{binding, invocation, "/broker/worker.sock", "/usr/bin/git", "/work", GitReadEndpoint{"/broker/git.sock", gitReadLocalURL},
			os.NewFile(3, "supervisor"), os.NewFile(4, "image"), os.NewFile(5, "manifest"), os.NewFile(6, "export")})
	}
	if len(args) != 4 {
		return errGitUnit
	}
	binding, err := parseGitProcessBinding(args[1], args[2])
	if err != nil {
		return errGitUnit
	}
	pid, err := strconv.Atoi(args[3])
	if err != nil || pid <= 0 || strconv.Itoa(pid) != args[3] {
		return errGitUnit
	}
	if gitProcessInheritedRoles(false) != nil {
		return errGitUnit
	}
	assets, err := receiveGitBootstrap(bounded, args[0], binding, invocation, pid, os.NewFile(3, "image"), os.NewFile(4, "supervisor"))
	if err != nil {
		return errGitUnit
	}
	defer assets.Close()
	return launchGitWorker(bounded, assets)
}

// Validate every raw role before constructing an owning os.File. On an invalid
// invocation these slots might instead be Go's newly created epoll/eventfd.
// Never close or change those while trying to reject a missing inheritance.
func gitProcessInheritedRoles(worker bool) error {
	return gitProcessInheritedRolesLimit(worker, gitGrantManifestBytes)
}

func gitProcessInheritedRolesLimit(worker bool, manifestLimit int64) error {
	image, supervisor := 3, 4
	if worker {
		image, supervisor = 4, 3
	}
	var stat unix.Stat_t
	seals, err := unix.FcntlInt(uintptr(image), unix.F_GET_SEALS, 0)
	if err != nil || seals&requiredStubSeals != requiredStubSeals || unix.Fstat(image, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&07777 != 0500 || stat.Size <= 0 || stat.Size > 64<<20 {
		return errGitUnit
	}
	var fs unix.Statfs_t
	if unix.Fstat(supervisor, &stat) != nil || stat.Mode&unix.S_IFMT != 0 || stat.Ino == 0 || unix.Fstatfs(supervisor, &fs) != nil || fs.Type != unix.PID_FS_MAGIC {
		return errGitUnit
	}
	poll := []unix.PollFd{{Fd: int32(supervisor), Events: unix.POLLIN}}
	if n, err := unix.Poll(poll, 0); err != nil || n != 0 || poll[0].Revents != 0 {
		return errGitUnit
	}
	last := 4
	if worker {
		seals, err := unix.FcntlInt(5, unix.F_GET_SEALS, 0)
		if err != nil || seals&requiredStubSeals != requiredStubSeals || unix.Fstat(5, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&07777 != 0400 || stat.Size <= 0 || stat.Size > manifestLimit {
			return errGitUnit
		}
		flags, err := unix.FcntlInt(6, unix.F_GETFL, 0)
		if err != nil || unix.Fstat(6, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFIFO || flags&unix.O_ACCMODE != unix.O_WRONLY {
			return errGitUnit
		}
		last = 6
	}
	if gitAuditInheritedFDs(last) != nil {
		return errGitUnit
	}
	for fd := 3; fd <= last; fd++ {
		unix.CloseOnExec(fd)
	}
	return nil
}
