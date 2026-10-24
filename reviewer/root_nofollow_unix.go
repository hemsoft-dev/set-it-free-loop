//go:build linux || darwin

package reviewer

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// openRootNoFollow opens name beneath root without following any symlink.
//
// os.Root.OpenFile confines resolution to the root but deliberately follows a
// symlink whose target stays inside it, even when the caller passes O_NOFOLLOW
// (Go 1.25 and 1.26; issue #257). This walks every component with openat and
// O_NOFOLLOW from the root's own directory descriptor, so the kernel refuses a
// symlink instead of resolving its target: ENOTDIR for an intermediate
// component (opened with O_DIRECTORY) or a final open that passes O_DIRECTORY,
// and ELOOP for any other final component. With
// O_PATH the final symlink itself is returned, never its target. O_CREATE
// without O_EXCL therefore cannot create the target of a dangling link either.
// Errors keep the kernel errno inside an *os.PathError for os.IsNotExist checks.
func openRootNoFollow(root *os.Root, name string, flag int, perm os.FileMode) (*os.File, error) {
	if name != "." && (!filepath.IsLocal(name) || filepath.Clean(name) != name) {
		return nil, &os.PathError{Op: "openat", Path: name, Err: syscall.EINVAL}
	}
	// "." is the root's own directory, never a link. Every component below it
	// is opened by the kernel with O_NOFOLLOW.
	anchor, err := root.OpenFile(".", os.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	defer anchor.Close()
	base := int(anchor.Fd())
	directory := base
	defer func() {
		if directory != base {
			unix.Close(directory)
		}
	}()
	parts := strings.Split(name, "/")
	for _, part := range parts[:len(parts)-1] {
		next, err := openatNoFollow(directory, part, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			return nil, &os.PathError{Op: "openat", Path: name, Err: err}
		}
		if directory != base {
			unix.Close(directory)
		}
		directory = next
	}
	fd, err := openatNoFollow(directory, parts[len(parts)-1], flag, uint32(perm.Perm()))
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), filepath.Join(root.Name(), name)), nil
}

func openatNoFollow(directory int, name string, flag int, perm uint32) (int, error) {
	for {
		fd, err := unix.Openat(directory, name, flag|unix.O_NOFOLLOW|unix.O_CLOEXEC, perm)
		if err != unix.EINTR {
			return fd, err
		}
	}
}
