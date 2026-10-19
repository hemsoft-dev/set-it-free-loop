package reviewer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

type Object struct {
	Mode string
	Data []byte
}

// ObjectStore runs outside the provider worker and holds no provider secrets.
type ObjectStore interface {
	Read(context.Context, string, string, int) (Object, error)
}

type ReadRecord struct {
	Revision, Path, Hash string
	Bytes                int
}

type Reader struct {
	mu          sync.Mutex
	store       ObjectStore
	snapshot    Snapshot
	paths       map[string]bool
	limits      Limits
	calls, used int
	reads       []ReadRecord
	coverage    []Coverage
}

func validPath(p string) bool {
	if p == "" || len(p) > 512 || !utf8.ValidString(p) || strings.ContainsAny(p, "\\\x00\r\n\t:") || strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." || part == ".git" || part == "." {
			return false
		}
	}
	return true
}

func NewReader(store ObjectStore, snapshot Snapshot, paths []string, limits Limits) (*Reader, error) {
	if store == nil {
		return nil, errors.New("missing object store")
	}
	if err := snapshot.Validate(); err != nil {
		return nil, err
	}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if len(paths) > 4096 {
		return nil, errors.New("too many source paths")
	}
	r := &Reader{store: store, snapshot: snapshot, paths: map[string]bool{}, limits: limits}
	for _, p := range paths {
		if !validPath(p) {
			return nil, errors.New("invalid source path")
		}
		r.paths[p] = true
	}
	return r, nil
}

func (r *Reader) Read(ctx context.Context, revision, p string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fail := func(reason string) (string, error) {
		loggedPath := p
		if !validPath(loggedPath) {
			loggedPath = ""
		}
		if len(r.coverage) < 256 {
			r.coverage = append(r.coverage, Coverage{loggedPath, reason})
		}
		return "", errors.New(reason)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if r.calls >= r.limits.ReaderCalls || r.used >= r.limits.ReaderBytes {
		return fail("reader limit exhausted")
	}
	r.calls++
	if !validPath(p) || !r.paths[p] {
		return fail("path outside job allowlist")
	}
	if revision != r.snapshot.Head && revision != r.snapshot.Base && revision != r.snapshot.MergeBase {
		return fail("revision outside job snapshot")
	}
	remaining := r.limits.ReaderBytes - r.used
	obj, err := r.store.Read(ctx, revision, p, remaining)
	if err != nil {
		return fail("missing or unreadable object")
	}
	if obj.Mode != "100644" && obj.Mode != "100755" {
		return fail("symlink, submodule or non-file omitted")
	}
	if len(obj.Data) > remaining {
		return fail("reader byte limit exceeded")
	}
	r.used += len(obj.Data)
	if bytes.IndexByte(obj.Data, 0) >= 0 || !utf8.Valid(obj.Data) {
		return fail("binary object omitted")
	}
	r.reads = append(r.reads, ReadRecord{revision, p, Digest(obj.Data), len(obj.Data)})
	return string(obj.Data), nil
}

func (r *Reader) Search(ctx context.Context, revision, needle string, maxResults int) ([]Anchor, error) {
	if needle == "" || len(needle) > 256 || strings.ContainsAny(needle, "\r\n") || maxResults < 1 || maxResults > 100 {
		return nil, errors.New("invalid bounded search")
	}
	paths := make([]string, 0, len(r.paths))
	for p := range r.paths {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var matches []Anchor
	for _, p := range paths {
		text, err := r.Read(ctx, revision, p)
		if err != nil {
			return matches, err
		}
		for i, line := range strings.Split(text, "\n") {
			if strings.Contains(line, needle) {
				matches = append(matches, Anchor{p, i + 1, line, revision})
				if len(matches) == maxResults {
					return matches, nil
				}
			}
		}
	}
	return matches, nil
}

func (r *Reader) Verify(ctx context.Context, a Anchor) error {
	if a.Line < 1 || a.Quote == "" || a.Revision != r.snapshot.Head {
		return errors.New("invalid source anchor")
	}
	s, err := r.Read(ctx, a.Revision, a.Path)
	if err != nil {
		return err
	}
	lines := strings.Split(s, "\n")
	if a.Line > len(lines) || lines[a.Line-1] != a.Quote {
		return errors.New("source quote does not match snapshot line")
	}
	return nil
}

// VerifyAll reads each source object once for a bounded group of citations.
// Validation must not exhaust the model's reader budget merely by rereading
// the same file separately for every cited line.
func (r *Reader) VerifyAll(ctx context.Context, anchors []Anchor) error {
	if len(anchors) > 1024 {
		return errors.New("too many source anchors")
	}
	groups := map[string][]Anchor{}
	for _, a := range anchors {
		if !validAnchor(a, r.snapshot) {
			return errors.New("invalid source anchor")
		}
		key := a.Revision + "\x00" + a.Path
		groups[key] = append(groups[key], a)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		group := groups[key]
		text, err := r.Read(ctx, group[0].Revision, group[0].Path)
		if err != nil {
			return err
		}
		lines := strings.Split(text, "\n")
		for _, a := range group {
			if a.Line > len(lines) || lines[a.Line-1] != a.Quote {
				return errors.New("source quote does not match snapshot line")
			}
		}
	}
	return nil
}

func (r *Reader) Evidence() ([]ReadRecord, []Coverage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ReadRecord(nil), r.reads...), append([]Coverage(nil), r.coverage...)
}

// GitStore reads immutable objects without checking out candidate code. Its
// caller supplies a trusted absolute git binary and bare/object repository path.
type GitStore struct{ Git, GitDir string }

type cappedOutput struct {
	bytes.Buffer
	max int
}

func (b *cappedOutput) Write(p []byte) (int, error) {
	if len(p) > b.max-b.Len() {
		return 0, errors.New("git output limit")
	}
	return b.Buffer.Write(p)
}

func (g GitStore) command(ctx context.Context, max int, args ...string) ([]byte, error) {
	if !filepath.IsAbs(g.Git) || !filepath.IsAbs(g.GitDir) || max <= 0 || max > 2<<20 {
		return nil, errors.New("invalid trusted git reader configuration")
	}
	args = append([]string{"--no-replace-objects", "--git-dir=" + g.GitDir}, args...)
	c := exec.CommandContext(ctx, g.Git, args...)
	c.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_NO_REPLACE_OBJECTS=1", "LC_ALL=C"}
	b := &cappedOutput{max: max}
	c.Stdout = b
	c.Stderr = &cappedOutput{max: 2048}
	if err := c.Run(); err != nil {
		return nil, errors.New("immutable git object unavailable or output limit exceeded")
	}
	return b.Bytes(), nil
}

func (g GitStore) Read(ctx context.Context, revision, p string, max int) (Object, error) {
	if !objectID.MatchString(revision) || !validPath(p) {
		return Object{}, errors.New("invalid immutable object lookup")
	}
	entry, err := g.command(ctx, 2048, "ls-tree", "-z", revision, "--", p)
	if err != nil {
		return Object{}, err
	}
	parts := bytes.SplitN(bytes.TrimSuffix(entry, []byte{0}), []byte{'\t'}, 2)
	if len(parts) != 2 || string(parts[1]) != p {
		return Object{}, errors.New("object path not found")
	}
	fields := strings.Fields(string(parts[0]))
	if len(fields) != 3 {
		return Object{}, errors.New("invalid tree entry")
	}
	if fields[0] != "100644" && fields[0] != "100755" {
		return Object{Mode: fields[0]}, nil
	}
	if !objectID.MatchString(fields[2]) {
		return Object{}, fmt.Errorf("invalid blob identity")
	}
	data, err := g.command(ctx, max, "cat-file", "blob", fields[2])
	return Object{fields[0], data}, err
}
