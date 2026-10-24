//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

var ErrGitQuarantine = errors.New("Git quarantine verification refused")

// GitAcquisitionBounds apply inside the measured acquisition unit. They do not
// replace its aggregate CPU/memory/process/disk boundary, including verification.
type GitAcquisitionBounds struct {
	Objects, Commits, Files               int
	ObjectBytes, ExpandedBytes, DiskBytes int64
}

func DefaultGitAcquisitionBounds() GitAcquisitionBounds {
	return GitAcquisitionBounds{16000, 8000, 32000, 32 << 20, 256 << 20, 128 << 20}
}
func (b GitAcquisitionBounds) valid() bool {
	max := DefaultGitAcquisitionBounds()
	return b.Objects > 0 && b.Objects <= max.Objects && b.Commits > 0 && b.Commits <= max.Commits && b.Files > 0 && b.Files <= max.Files && b.ObjectBytes > 0 && b.ObjectBytes <= max.ObjectBytes && b.ExpandedBytes > 0 && b.ExpandedBytes <= max.ExpandedBytes && b.DiskBytes > 0 && b.DiskBytes <= max.DiskBytes
}

type GitAcquisitionInventory struct {
	Version                   int
	SnapshotHash, ObjectsHash string
	Objects, Commits, Files   int
	ExpandedBytes, DiskBytes  int64
}

func gitAcquisitionConfig(s Snapshot) string {
	format, version := "", "0"
	if len(s.Head) == 64 {
		version = "1"
		format = "[extensions]\n\tobjectformat = sha256\n"
	}
	return "[core]\n\trepositoryformatversion = " + version + "\n\tbare = true\n\tlogallrefupdates = false\n\thooksPath = /dev/null\n\tfsmonitor = false\n\tcommitGraph = false\n\tmultiPackIndex = false\n" + format
}
func gitQuarantineHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, ch := range []byte(value) {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}
func gitQuarantinePath(name string, directory bool, hashLength int) bool {
	if directory {
		switch name {
		case ".", "objects", "objects/info", "objects/pack", "refs", "refs/sfl", "refs/heads", "refs/tags":
			return true
		}
		parts := strings.Split(name, "/")
		return len(parts) == 2 && parts[0] == "objects" && gitQuarantineHex(parts[1], 2)
	}
	switch name {
	case "HEAD", "config", "FETCH_HEAD", "refs/sfl/head", "refs/sfl/base":
		return true
	}
	parts := strings.Split(name, "/")
	if len(parts) != 3 || parts[0] != "objects" {
		return false
	}
	if parts[1] != "pack" {
		return gitQuarantineHex(parts[1], 2) && gitQuarantineHex(parts[2], hashLength-2)
	}
	stem, extension, ok := strings.Cut(parts[2], ".")
	return ok && strings.HasPrefix(stem, "pack-") && gitQuarantineHex(strings.TrimPrefix(stem, "pack-"), hashLength) && (extension == "pack" || extension == "idx" || extension == "rev")
}
func gitQuarantineFiles(root *os.Root, s Snapshot, b GitAcquisitionBounds) (int, int64, error) {
	count := 0
	var size int64
	var visit func(*os.Root, string, int) error
	visit = func(directory *os.Root, prefix string, depth int) error {
		if depth > 8 {
			return ErrGitQuarantine
		}
		file, err := directory.Open(".")
		if err != nil {
			return ErrGitQuarantine
		}
		defer file.Close()
		for {
			entries, err := file.ReadDir(64)
			if err != nil && err != io.EOF {
				return ErrGitQuarantine
			}
			for _, entry := range entries {
				count++
				if count > b.Files {
					return ErrGitQuarantine
				}
				name := entry.Name()
				relative := name
				if prefix != "" {
					relative = prefix + "/" + name
				}
				info, err := directory.Lstat(name)
				if err != nil || !gitQuarantinePath(relative, info.IsDir(), len(s.Head)) {
					return ErrGitQuarantine
				}
				stat, ok := info.Sys().(*syscall.Stat_t)
				if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode()&os.ModeSymlink != 0 {
					return ErrGitQuarantine
				}
				if info.IsDir() {
					child, err := directory.OpenRoot(name)
					if err != nil {
						return ErrGitQuarantine
					}
					opened, statErr := child.Stat(".")
					if statErr != nil || !os.SameFile(info, opened) {
						child.Close()
						return ErrGitQuarantine
					}
					err = visit(child, relative, depth+1)
					child.Close()
					if err != nil {
						return err
					}
				} else {
					if !info.Mode().IsRegular() || stat.Nlink != 1 || info.Size() < 0 || info.Size() > b.DiskBytes-size {
						return ErrGitQuarantine
					}
					size += info.Size()
				}
			}
			if err == io.EOF {
				return nil
			}
		}
	}
	err := visit(root, "", 0)
	return count, size, err
}
func gitQuarantineFileEquals(root *os.Root, name string, want string) bool {
	file, err := openRootNoFollow(root, name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, int64(len(want)+1)))
	return err == nil && bytes.Equal(raw, []byte(want))
}

// verifyGitQuarantine is worker-side plumbing, not a host qualification API.
// Every command parses untrusted Git objects and MUST run within the measured
// acquisition unit's aggregate resource/filesystem/egress boundary. The factory
// must bind the input, executable and receipt, stop all writers and the broker,
// and verify whole-unit stop before publishing any retained object store.
// Current direct tests use only trusted synthetic repos and prove algorithms.
func verifyGitQuarantine(ctx context.Context, store GitStore, s Snapshot, b GitAcquisitionBounds) (GitAcquisitionInventory, error) {
	zero := GitAcquisitionInventory{}
	if ctx == nil || ctx.Err() != nil || s.Validate() != nil || !b.valid() || !filepath.IsAbs(store.Git) || !filepath.IsAbs(store.GitDir) || filepath.Clean(store.GitDir) != store.GitDir || privateDirectory(store.GitDir) != nil {
		return zero, ErrGitQuarantine
	}
	resolved, err := filepath.EvalSymlinks(store.GitDir)
	if err != nil || resolved != store.GitDir {
		return zero, ErrGitQuarantine
	}
	root, err := os.OpenRoot(store.GitDir)
	if err != nil {
		return zero, ErrGitQuarantine
	}
	defer root.Close()
	files, disk, err := gitQuarantineFiles(root, s, b)
	if err != nil {
		return zero, ErrGitQuarantine
	}
	if !gitQuarantineFileEquals(root, "config", gitAcquisitionConfig(s)) || !gitQuarantineFileEquals(root, "HEAD", s.Head+"\n") || !gitQuarantineFileEquals(root, "refs/sfl/head", s.Head+"\n") || !gitQuarantineFileEquals(root, "refs/sfl/base", s.Base+"\n") {
		return zero, ErrGitQuarantine
	}
	run := func(max int, args ...string) ([]byte, error) {
		fixed := []string{"-c", "core.commitGraph=false", "-c", "core.multiPackIndex=false", "-c", "gc.auto=0", "-c", "maintenance.auto=false"}
		return store.command(ctx, max, append(fixed, args...)...)
	}
	metadata, err := run(2<<20, "cat-file", "--batch-all-objects", "--batch-check=%(objectname) %(objecttype) %(objectsize)")
	if err != nil || len(metadata) == 0 {
		return zero, ErrGitQuarantine
	}
	objects := map[string]string{}
	rows := []string{}
	commits := 0
	var expanded int64
	for _, line := range strings.Split(strings.TrimSuffix(string(metadata), "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || len(fields[0]) != len(s.Head) || !objectID.MatchString(fields[0]) || objects[fields[0]] != "" {
			return zero, ErrGitQuarantine
		}
		switch fields[1] {
		case "commit", "tree", "blob", "tag":
		default:
			return zero, ErrGitQuarantine
		}
		amount, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || amount < 0 || amount > b.ObjectBytes || amount > b.ExpandedBytes-expanded {
			return zero, ErrGitQuarantine
		}
		expanded += amount
		objects[fields[0]] = fields[1]
		rows = append(rows, line)
		if fields[1] == "commit" {
			commits++
		}
		if len(objects) > b.Objects || commits > b.Commits {
			return zero, ErrGitQuarantine
		}
	}
	for _, id := range []string{s.Head, s.Base, s.MergeBase} {
		if objects[id] != "commit" {
			return zero, ErrGitQuarantine
		}
	}
	if _, err := run(2048, "fsck", "--full", "--strict", "--no-reflogs", "--no-dangling"); err != nil {
		return zero, ErrGitQuarantine
	}
	graph, err := run(2<<20, "rev-list", "--parents", s.Base, s.Head, "--")
	if err != nil || len(graph) == 0 {
		return zero, ErrGitQuarantine
	}
	parents := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(graph), "\n"), "\n") {
		ids := strings.Fields(line)
		if len(ids) == 0 || objects[ids[0]] != "commit" {
			return zero, ErrGitQuarantine
		}
		if _, duplicate := parents[ids[0]]; duplicate {
			return zero, ErrGitQuarantine
		}
		parents[ids[0]] = ids[1:]
		for _, id := range ids[1:] {
			if objects[id] != "commit" {
				return zero, ErrGitQuarantine
			}
		}
	}
	if _, ok := parents[s.Head]; !ok {
		return zero, ErrGitQuarantine
	}
	if _, ok := parents[s.Base]; !ok {
		return zero, ErrGitQuarantine
	}
	for _, ids := range parents {
		for _, id := range ids {
			if _, complete := parents[id]; !complete {
				return zero, ErrGitQuarantine
			}
		}
	}
	merge, err := run(256, "merge-base", "--all", s.Base, s.Head)
	if err != nil || string(merge) != s.MergeBase+"\n" || ctx.Err() != nil {
		return zero, ErrGitQuarantine
	}
	sort.Strings(rows)
	return GitAcquisitionInventory{1, Digest(s), Digest(rows), len(objects), commits, files, expanded, disk}, nil
}
