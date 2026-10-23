//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func gitRuntimeExtraFiles(metadata []byte) []nativeRuntimeFile {
	return []nativeRuntimeFile{{"dev/null", gitRawHash(nil), 0}, {"dev/urandom", gitRawHash(nil), 0}, {gitRuntimeMetadata, gitRawHash(metadata), int64(len(metadata))}}
}

// verifyGitRuntimeDirectory proves the inherited directory reconstructs the
// pinned runtime artifact, including every library byte and no extra paths.
// Only trusted release assembly happens on this host filesystem. Candidate
// Git objects must still reside on the independently qualified bounded tmpfs.
// The same-UID supervisor owns this directory until whole-unit stop; no parser
// may inherit its host FD. This does not attest the later read-only mount.
func verifyGitRuntimeDirectory(ctx context.Context, directory *os.File, pins GitRuntimePins) (gitRuntimeManifest, error) {
	zero := gitRuntimeManifest{}
	if ctx == nil || ctx.Err() != nil || directory == nil {
		return zero, errGitRuntime
	}
	retained, err := duplicateGitControlFile(directory)
	if err != nil {
		return zero, errGitRuntime
	}
	defer retained.Close()
	info, err := retained.Stat()
	stat, ok := infoSysGitDirectory(info)
	var fs unix.Statfs_t
	if err != nil || !ok || !info.IsDir() || info.Mode().Perm() != 0555 || stat.Uid != uint32(os.Geteuid()) || unix.Fstatfs(int(retained.Fd()), &fs) != nil || fs.Flags&unix.ST_NOEXEC != 0 {
		return zero, errGitRuntime
	}
	// Open through the retained kernel descriptor, not a mutable host pathname.
	root, err := os.OpenRoot(fmt.Sprintf("/proc/self/fd/%d", retained.Fd()))
	if err != nil {
		return zero, errGitRuntime
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return zero, errGitRuntime
	}
	file, err := openRootNoFollow(root, gitRuntimeMetadata, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return zero, errGitRuntime
	}
	metaInfo, err := file.Stat()
	if err != nil || !metaInfo.Mode().IsRegular() || metaInfo.Size() <= 0 || metaInfo.Size() > gitRuntimeManifestBytes {
		file.Close()
		return zero, errGitRuntime
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, gitRuntimeManifestBytes+1))
	closeErr := file.Close()
	m, decodeErr := DecodeStrict[gitRuntimeManifest](raw, gitRuntimeManifestBytes)
	canonical, marshalErr := json.Marshal(m)
	if readErr != nil || closeErr != nil || decodeErr != nil || marshalErr != nil || !bytes.Equal(raw, canonical) || !m.valid(pins) {
		return zero, errGitRuntime
	}
	directories := map[string]bool{".": true, "proc": true, "dev": true, "work": true, "broker": true, "tmp": true}
	files := append(append([]nativeRuntimeFile{}, m.Files...), gitRuntimeExtraFiles(raw)...)
	for _, file := range files {
		parent := filepath.Dir(file.Path)
		for parent != "." {
			directories[parent] = true
			parent = filepath.Dir(parent)
		}
		info, err := root.Lstat(file.Path)
		mode := os.FileMode(0444)
		if file.Path == gitRuntimeProgram || file.Path == gitRuntimeHTTP || file.Path == gitRuntimeLoader {
			mode = 0555
		}
		if err != nil || info.Mode().Perm() != mode || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return zero, errGitRuntime
		}
	}
	for path := range directories {
		info, err := root.Lstat(path)
		stat, ok := infoSysGitDirectory(info)
		if err != nil || !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return zero, errGitRuntime
		}
	}
	// A local verifier uses the anchored root as its own parent. Do not call
	// Close on this ownership-shaped view; it must never delete caller files.
	view := &nativeRuntimeRoot{parent: root, root: root, directory: ".", identity: info, directories: directories, files: files, ready: true}
	if view.verify() != nil {
		return zero, errGitRuntime
	}
	hash := sha256.New()
	hash.Write([]byte(gitRuntimeMagic))
	if binary.Write(hash, binary.BigEndian, uint32(len(raw))) != nil {
		return zero, errGitRuntime
	}
	hash.Write(raw)
	var size int64 = int64(len(gitRuntimeMagic) + 4 + len(raw))
	buffer := make([]byte, 32<<10)
	for _, pin := range m.Files {
		file, err := openRootNoFollow(root, pin.Path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return zero, errGitRuntime
		}
		reader := io.NewSectionReader(file, 0, pin.Bytes+1)
		var count int64
		for {
			if ctx.Err() != nil {
				file.Close()
				return zero, errGitRuntime
			}
			n, readErr := reader.Read(buffer)
			if n > 0 {
				count += int64(n)
				hash.Write(buffer[:n])
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				file.Close()
				return zero, errGitRuntime
			}
		}
		if file.Close() != nil || count != pin.Bytes {
			return zero, errGitRuntime
		}
		size += count
	}
	if size != pins.Artifact.Bytes || hex.EncodeToString(hash.Sum(nil)) != pins.Artifact.SHA256 || ctx.Err() != nil {
		return zero, errGitRuntime
	}
	return m, nil
}

func infoSysGitDirectory(info os.FileInfo) (*syscall.Stat_t, bool) {
	if info == nil {
		return nil, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return stat, ok
}
