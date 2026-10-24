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

// gitQuarantineFiles enumerates the complete repository layout. A file that
// would take the total over DiskBytes sets over; enumeration still checks the
// rest of the layout, so an overrun is reported only for an otherwise valid
// repository. The filesystem entry bound stays an opaque failure: with
// fetch.unpackLimit=1 the layout of an honest fetch is a few fixed paths and
// one pack, so too many entries never describe the fetched history.
func gitQuarantineFiles(root *os.Root, s Snapshot, b GitAcquisitionBounds) (int, int64, bool, error) {
	count := 0
	var size int64
	over := false
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
					if !info.Mode().IsRegular() || stat.Nlink != 1 || info.Size() < 0 {
						return ErrGitQuarantine
					}
					if over || info.Size() > b.DiskBytes-size {
						over = true
					} else {
						size += info.Size()
					}
				}
			}
			if err == io.EOF {
				return nil
			}
		}
	}
	err := visit(root, "", 0)
	return count, size, over, err
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

// gitQuarantineMetadata is the parsed cat-file --batch-all-objects inventory.
type gitQuarantineMetadata struct {
	objects  map[string]string
	rows     []string
	commits  int
	expanded int64
}

// parseGitQuarantineMetadata validates every complete line of raw and counts
// it against b. truncated marks raw as the exact prefix of output that was
// over its ceiling; its last partial line is dropped. over reports a definite
// object, commit, object-byte or expanded-byte overrun among well-formed
// lines. Any malformed or duplicate line is an error, even after an overrun,
// and a truncated inventory without an overrun is an error too, since it is
// no complete answer.
func parseGitQuarantineMetadata(raw []byte, truncated bool, s Snapshot, b GitAcquisitionBounds) (gitQuarantineMetadata, bool, error) {
	m := gitQuarantineMetadata{objects: map[string]string{}}
	if truncated {
		end := bytes.LastIndexByte(raw, '\n')
		if end < 0 {
			return gitQuarantineMetadata{}, false, ErrGitQuarantine
		}
		raw = raw[:end+1]
	}
	if len(raw) == 0 {
		return gitQuarantineMetadata{}, false, ErrGitQuarantine
	}
	over := false
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || len(fields[0]) != len(s.Head) || !objectID.MatchString(fields[0]) || m.objects[fields[0]] != "" {
			return gitQuarantineMetadata{}, false, ErrGitQuarantine
		}
		switch fields[1] {
		case "commit", "tree", "blob", "tag":
		default:
			return gitQuarantineMetadata{}, false, ErrGitQuarantine
		}
		amount, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || amount < 0 {
			return gitQuarantineMetadata{}, false, ErrGitQuarantine
		}
		if over || amount > b.ObjectBytes || amount > b.ExpandedBytes-m.expanded {
			over = true
		} else {
			m.expanded += amount
		}
		m.objects[fields[0]] = fields[1]
		m.rows = append(m.rows, line)
		if fields[1] == "commit" {
			m.commits++
		}
		if len(m.objects) > b.Objects || m.commits > b.Commits {
			over = true
		}
	}
	if truncated && !over {
		return gitQuarantineMetadata{}, false, ErrGitQuarantine
	}
	return m, over, nil
}

// gitQuarantineOverrun answers a definite bound overrun as the permanent
// history_limit refusal. A cancelled or expired context is never an answer.
func gitQuarantineOverrun(ctx context.Context) error {
	if ctx.Err() != nil {
		return ErrGitQuarantine
	}
	return CaptureRefusal{CaptureHistoryLimit}
}

// verifyGitQuarantine is worker-side plumbing, not a host qualification API.
// Every command parses untrusted Git objects and MUST run within the measured
// acquisition unit's aggregate resource/filesystem/egress boundary. The factory
// must bind the input, executable and receipt, stop all writers and the broker,
// and verify whole-unit stop before publishing any retained object store.
// Current direct tests use only trusted synthetic repos and prove algorithms.
// After every other check passes, Git's definite answer that the snapshot's
// merge base is missing, ambiguous or different returns a CaptureRefusal.
// A definite overrun of an object, commit or byte bound, by a well-formed
// layout or inventory, returns the history_limit refusal.
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
	files, disk, diskOver, err := gitQuarantineFiles(root, s, b)
	if err != nil {
		return zero, ErrGitQuarantine
	}
	if !gitQuarantineFileEquals(root, "config", gitAcquisitionConfig(s)) || !gitQuarantineFileEquals(root, "HEAD", s.Head+"\n") || !gitQuarantineFileEquals(root, "refs/sfl/head", s.Head+"\n") || !gitQuarantineFileEquals(root, "refs/sfl/base", s.Base+"\n") {
		return zero, ErrGitQuarantine
	}
	// A complete, valid layout over the disk bound is a definite answer about
	// the fetched pack. No Git command parses an oversized repository.
	if diskOver {
		return zero, gitQuarantineOverrun(ctx)
	}
	fixed := []string{"-c", "core.commitGraph=false", "-c", "core.multiPackIndex=false", "-c", "gc.auto=0", "-c", "maintenance.auto=false"}
	run := func(max int, args ...string) ([]byte, error) {
		return store.command(ctx, max, append(fixed, args...)...)
	}
	// Output over the 2-MiB ceiling keeps its exact prefix. Even the longest
	// well-formed line within the object-byte bound fits more than 25,000
	// times in that prefix, over the largest object bound, so a well-formed
	// prefix is itself a definite object overrun.
	metadata, err := store.boundedCommand(ctx, 2<<20, append(fixed, "cat-file", "--batch-all-objects", "--batch-check=%(objectname) %(objecttype) %(objectsize)")...)
	truncated := errors.Is(err, errGitOutputLimit) && ctx.Err() == nil
	if err != nil && !truncated {
		return zero, ErrGitQuarantine
	}
	inventory, over, err := parseGitQuarantineMetadata(metadata, truncated, s, b)
	if err != nil {
		return zero, ErrGitQuarantine
	}
	if over {
		return zero, gitQuarantineOverrun(ctx)
	}
	objects, rows, commits, expanded := inventory.objects, inventory.rows, inventory.commits, inventory.expanded
	for _, id := range []string{s.Head, s.Base} {
		if objects[id] != "commit" {
			return zero, ErrGitQuarantine
		}
	}
	// A recorded merge base absent from the store is answered only after the
	// fetched history is proved complete below; any other object type refuses.
	mergeBase := objects[s.MergeBase]
	if mergeBase != "commit" && mergeBase != "" {
		return zero, ErrGitQuarantine
	}
	if _, err := run(2048, "fsck", "--full", "--strict", "--no-reflogs", "--no-dangling"); err != nil {
		return zero, ErrGitQuarantine
	}
	graph, err := run(2<<20, "rev-list", "--parents", s.Base, s.Head, "--")
	if errors.Is(err, errGitOutputLimit) && ctx.Err() == nil {
		// The objects passed strict fsck and the commit bound, so a parent
		// graph over its ceiling is a definite answer about this history.
		return zero, gitQuarantineOverrun(ctx)
	}
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
		// Head and base history is complete and verified, so Git's definite
		// answer about it is permanent for this snapshot. A recorded merge base
		// outside that history is missing; anything else stays opaque.
		if ctx.Err() == nil && mergeBase == "" {
			return zero, CaptureRefusal{CaptureMissingHistory}
		}
		if refusal, ok := snapshotMergeBaseRefusal(ctx, merge, err, len(s.Head)); ok {
			return zero, refusal
		}
		return zero, ErrGitQuarantine
	}
	sort.Strings(rows)
	return GitAcquisitionInventory{1, Digest(s), Digest(rows), len(objects), commits, files, expanded, disk}, nil
}
