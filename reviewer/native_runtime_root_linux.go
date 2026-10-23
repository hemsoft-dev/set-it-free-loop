//go:build linux

package reviewer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// nativeRuntimeRoot is a single-owner prepared filesystem. Its descriptor may
// be consumed by the namespace helper; it must never survive into QEMU or Git. Close
// belongs after the owned process has stopped, not to a model/tool callback.
type nativeRuntimeRoot struct {
	parent, root  *os.Root
	directory     string
	identity      os.FileInfo
	directories   map[string]bool
	files         []nativeRuntimeFile
	ready, closed bool
}

const nativeRuntimeDirectory = "qemu-runtime"

func prepareNativeRuntime(parentPath string, input io.Reader, program VMAssetPin, programData io.Reader) (_ *nativeRuntimeRoot, result error) {
	return prepareNativeRuntimeOwned(parentPath, input, program, programData, nil)
}

func prepareNativeRuntimeOwned(parentPath string, input io.Reader, program VMAssetPin, programData io.Reader, created func(*nativeRuntimeRoot) error) (_ *nativeRuntimeRoot, result error) {
	if programData == nil || !filepath.IsAbs(parentPath) || filepath.Clean(parentPath) != parentPath || privateDirectory(parentPath) != nil {
		return nil, fmt.Errorf("%w: parent qualification", ErrNativeRuntime)
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return nil, fmt.Errorf("%w: parent open", ErrNativeRuntime)
	}
	if parent.Mkdir(nativeRuntimeDirectory, 0700) != nil {
		parent.Close()
		return nil, fmt.Errorf("%w: root creation", ErrNativeRuntime)
	}
	identity, err := parent.Lstat(nativeRuntimeDirectory)
	if err != nil {
		parent.Close()
		return nil, fmt.Errorf("%w: root identity intake", ErrNativeRuntime)
	}
	root, err := parent.OpenRoot(nativeRuntimeDirectory)
	if err != nil {
		parent.Close()
		return nil, fmt.Errorf("%w: root open", ErrNativeRuntime)
	}
	prepared := &nativeRuntimeRoot{parent: parent, root: root, identity: identity, directories: map[string]bool{".": true}}
	defer func() {
		if result != nil {
			result = errors.Join(result, prepared.Close())
		}
	}()
	if created != nil {
		if err := created(prepared); err != nil {
			return nil, err
		}
	}
	for _, dir := range []string{"proc", "dev", "tmp"} {
		prepared.directories[dir] = true
		if root.Mkdir(dir, 0700) != nil {
			return nil, fmt.Errorf("%w: mountpoint creation", ErrNativeRuntime)
		}
	}
	writeFile := func(file nativeRuntimeFile, body io.Reader) error {
		parts := strings.Split(filepath.Dir(file.Path), "/")
		for index := range parts {
			prepared.directories[strings.Join(parts[:index+1], "/")] = true
		}
		if root.MkdirAll(filepath.Dir(file.Path), 0700) != nil {
			return ErrNativeRuntime
		}
		output, err := openRootNoFollow(root, file.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|unix.O_NOFOLLOW, 0600)
		if err != nil {
			return ErrNativeRuntime
		}
		hash := sha256.New()
		count, copyErr := io.Copy(output, io.TeeReader(io.LimitReader(body, file.Bytes+1), hash))
		syncErr := output.Sync()
		closeErr := output.Close()
		if copyErr != nil || syncErr != nil || closeErr != nil || count != file.Bytes || hex.EncodeToString(hash.Sum(nil)) != file.SHA256 {
			return ErrNativeRuntime
		}
		mode := os.FileMode(0444)
		if file.Path == "lib64/ld-linux-x86-64.so.2" || file.Path == "qemu" {
			mode = 0555
		}
		return root.Chmod(file.Path, mode)
	}
	manifest, err := streamNativeRuntime(input, program, writeFile)
	if err != nil {
		return nil, fmt.Errorf("%w: artifact stream", ErrNativeRuntime)
	}
	programFile := nativeRuntimeFile{Path: "qemu", SHA256: program.SHA256, Bytes: program.Bytes}
	if writeFile(programFile, programData) != nil {
		return nil, fmt.Errorf("%w: program copy", ErrNativeRuntime)
	}
	prepared.files = append(manifest.Files, programFile)
	for dir := range prepared.directories {
		if root.Chmod(dir, 0555) != nil {
			return nil, fmt.Errorf("%w: directory hardening", ErrNativeRuntime)
		}
	}
	prepared.ready = true
	if err := prepared.verify(); err != nil {
		return nil, err
	}
	return prepared, nil
}

func (r *nativeRuntimeRoot) directoryName() string {
	if r.directory == "" {
		return nativeRuntimeDirectory
	}
	return r.directory
}
func (r *nativeRuntimeRoot) verify() error {
	if r == nil || r.closed || !r.ready {
		return fmt.Errorf("%w: not ready", ErrNativeRuntime)
	}
	current, err := r.parent.Lstat(r.directoryName())
	if err != nil || !os.SameFile(current, r.identity) {
		return fmt.Errorf("%w: root identity", ErrNativeRuntime)
	}
	expected := map[string]bool{}
	for dir := range r.directories {
		expected[dir] = true
	}
	for _, file := range r.files {
		expected[file.Path] = true
	}
	for dir := range r.directories {
		info, err := r.root.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0555 {
			return fmt.Errorf("%w: directory metadata", ErrNativeRuntime)
		}
		opened, err := r.root.Open(dir)
		if err != nil {
			return fmt.Errorf("%w: directory open", ErrNativeRuntime)
		}
		entries, readErr := opened.ReadDir(len(expected) + 1)
		opened.Close()
		if readErr != nil && readErr != io.EOF {
			return fmt.Errorf("%w: directory read", ErrNativeRuntime)
		}
		if len(entries) > len(expected) {
			return fmt.Errorf("%w: directory count", ErrNativeRuntime)
		}
		for _, entry := range entries {
			if !expected[filepath.Join(dir, entry.Name())] {
				return fmt.Errorf("%w: unexpected path", ErrNativeRuntime)
			}
		}
	}
	for _, pin := range r.files {
		file, err := openRootNoFollow(r.root, pin.Path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return fmt.Errorf("%w: file open", ErrNativeRuntime)
		}
		info, err := file.Stat()
		valid := err == nil && info.Mode().IsRegular() && info.Size() == pin.Bytes && info.Mode().Perm()&0222 == 0
		if valid {
			st, ok := info.Sys().(*syscall.Stat_t)
			valid = ok && st.Uid == uint32(os.Geteuid()) && st.Nlink == 1
		}
		if !valid {
			file.Close()
			return fmt.Errorf("%w: file metadata", ErrNativeRuntime)
		}
		hash := sha256.New()
		n, readErr := io.Copy(hash, io.LimitReader(file, pin.Bytes+1))
		file.Close()
		if !valid || readErr != nil || n != pin.Bytes || hex.EncodeToString(hash.Sum(nil)) != pin.SHA256 {
			return fmt.Errorf("%w: file bytes", ErrNativeRuntime)
		}
	}
	return nil
}

func (r *nativeRuntimeRoot) directoryFD() (*os.File, error) {
	if r.verify() != nil {
		return nil, ErrNativeRuntime
	}
	file, err := openRootNoFollow(r.root, ".", os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrNativeRuntime
	}
	return file, nil
}

func (r *nativeRuntimeRoot) Close() error {
	if r == nil || r.closed {
		return nil
	}
	r.closed = true
	defer r.parent.Close()
	defer r.root.Close()
	current, err := r.parent.Lstat(r.directoryName())
	if err != nil || !os.SameFile(current, r.identity) {
		return ErrNativeRuntime
	}
	// Restore write permission only inside this owned root, after process stop.
	directories := make([]string, 0, len(r.directories))
	for dir := range r.directories {
		directories = append(directories, dir)
	}
	sort.Strings(directories)
	for _, dir := range directories {
		if err := r.root.Chmod(dir, 0700); err != nil && !os.IsNotExist(err) {
			return ErrNativeRuntime
		}
	}
	if r.parent.RemoveAll(r.directoryName()) != nil {
		return ErrNativeRuntime
	}
	return nil
}
