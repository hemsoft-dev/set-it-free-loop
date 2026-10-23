package reviewer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type fixtureStore struct {
	files map[string]Object
	calls int
}

func (s *fixtureStore) Read(_ context.Context, revision, p string, max int) (Object, error) {
	s.calls++
	o, ok := s.files[p]
	if !ok {
		return Object{}, errors.New("missing")
	}
	return o, nil
}

func TestReaderRejectsEscapeAndRecordsCoverage(t *testing.T) {
	s := &fixtureStore{files: map[string]Object{"safe.go": {"100644", []byte("safe\nline")}, "link": {"120000", []byte("/secret-canary")}, "sub": {"160000", nil}, "binary": {"100644", []byte{0, 1}}}}
	r, err := NewReader(s, testSnapshot(), []string{"safe.go", "link", "sub", "binary", "missing"}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../secret", "/secret", "safe.go/../secret", ".git/config", "dir\\secret", "unknown"} {
		if _, err := r.Read(context.Background(), testSnapshot().Head, p); err == nil {
			t.Fatal("path escape", p)
		}
	}
	if s.calls != 0 {
		t.Fatal("escape reached object store")
	}
	if _, err := r.Read(context.Background(), strings.Repeat("f", 40), "safe.go"); err == nil {
		t.Fatal("cross-job revision")
	}
	for _, p := range []string{"link", "sub", "binary", "missing"} {
		if _, err := r.Read(context.Background(), testSnapshot().Head, p); err == nil {
			t.Fatal("omission treated as readable", p)
		}
	}
	if err := r.Verify(context.Background(), Anchor{"safe.go", 2, "line", testSnapshot().Head}); err != nil {
		t.Fatal(err)
	}
	if err := r.Verify(context.Background(), Anchor{"safe.go", 2, "forged", testSnapshot().Head}); err == nil {
		t.Fatal("forged quote")
	}
	reads, coverage := r.Evidence()
	if len(reads) != 2 || len(coverage) != 11 {
		t.Fatalf("reads=%d coverage=%d", len(reads), len(coverage))
	}
}

func TestReaderBoundedMemoryAndSearch(t *testing.T) {
	l := DefaultLimits()
	l.ReaderCalls = 2
	l.ReaderBytes = 8
	s := &fixtureStore{files: map[string]Object{"a.go": {"100644", []byte("one\ntwo")}}}
	r, _ := NewReader(s, testSnapshot(), []string{"a.go"}, l)
	m, err := r.Search(context.Background(), testSnapshot().Head, "two", 1)
	if err != nil || len(m) != 1 || m[0].Line != 2 {
		t.Fatalf("%+v %v", m, err)
	}
	if _, err := r.Read(context.Background(), testSnapshot().Head, "a.go"); err == nil {
		t.Fatal("byte bound bypass")
	}
	for i := 0; i < 300; i++ {
		r.Read(context.Background(), testSnapshot().Head, strings.Repeat("X", 1024))
	}
	_, coverage := r.Evidence()
	if len(coverage) > 256 {
		t.Fatal("unbounded coverage log")
	}
	for _, c := range coverage {
		if len(c.Path) > 512 {
			t.Fatal("unbounded path in log")
		}
	}
}

func TestGitReaderUsesImmutableObjectsWithoutCheckoutOrHooks(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git required")
	}
	git, err = filepath.Abs(git)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		c := exec.Command(git, args...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	run("config", "user.name", "Fixture")
	run("config", "user.email", "fixture@example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "source.go"), []byte("original source"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/secret-canary", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.go"), []byte(strings.Repeat("x", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "source.go", "link", "big.go")
	run("commit", "-qm", "fixture")
	head := run("rev-parse", "HEAD")
	os.WriteFile(filepath.Join(dir, "source.go"), []byte("candidate checkout changed"), 0600)
	marker := filepath.Join(dir, "hook-executed")
	os.WriteFile(filepath.Join(dir, ".git", "hooks", "post-checkout"), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0700)
	snap := testSnapshot()
	snap.Head = head
	snap.Base = head
	snap.MergeBase = head
	r, err := NewReader(GitStore{git, filepath.Join(dir, ".git")}, snap, []string{"source.go", "link"}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	v, err := r.Read(context.Background(), head, "source.go")
	if err != nil || v != "original source" {
		t.Fatalf("%q %v", v, err)
	}
	if _, err := r.Read(context.Background(), head, "link"); err == nil {
		t.Fatal("followed symlink")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("candidate hook executed")
	}
	// Git's capped blob output is the byte budget, not a missing object. The
	// cap must hold even though the output buffer could read from the pipe.
	if object, err := (GitStore{git, filepath.Join(dir, ".git")}).Read(context.Background(), head, "big.go", 32); err == nil || len(object.Data) != 0 {
		t.Fatal("git output cap bypassed", len(object.Data), err)
	}
	limits := DefaultLimits()
	limits.ReaderBytes = 32
	r, err = NewReader(GitStore{git, filepath.Join(dir, ".git")}, snap, []string{"big.go", "gone.go"}, limits)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"big.go", "gone.go"} {
		if _, err := r.Read(context.Background(), head, p); err == nil {
			t.Fatal("unbounded or missing object served", p)
		}
	}
	reads, coverage := r.Evidence()
	want := []Coverage{{"big.go", "source byte limit exhausted"}, {"gone.go", "missing or unreadable object"}}
	if len(reads) != 0 || fmt.Sprint(coverage) != fmt.Sprint(want) {
		t.Fatal("byte budget refusal not told apart from a missing object", reads, coverage)
	}
}

// A byte-budget refusal is recorded as such, never as a missing object, so
// coverage tells reader-limit exhaustion apart from an absent file.
func TestReaderRecordsByteBudgetApartFromMissingObjects(t *testing.T) {
	l := DefaultLimits()
	l.ReaderCalls = 8
	l.ReaderBytes = 8
	s := &fixtureStore{files: map[string]Object{"a.go": {"100644", []byte("one\nt")}, "b.go": {"100644", []byte("two")}, "big.go": {"100644", []byte("123456789")}}}
	r, err := NewReader(s, testSnapshot(), []string{"a.go", "b.go", "big.go", "gone.go"}, l)
	if err != nil {
		t.Fatal(err)
	}
	head := testSnapshot().Head
	for _, step := range []struct {
		path string
		ok   bool
	}{{"big.go", false}, {"gone.go", false}, {"a.go", true}, {"a.go", false}, {"b.go", true}, {"b.go", false}} {
		if _, err := r.Read(context.Background(), head, step.path); (err == nil) != step.ok {
			t.Fatal("unexpected read outcome", step.path, err)
		}
	}
	reads, coverage := r.Evidence()
	budget := "source byte limit exhausted"
	want := []Coverage{{"big.go", budget}, {"gone.go", "missing or unreadable object"}, {"a.go", budget}, {"b.go", budget}}
	if len(reads) != 2 || fmt.Sprint(coverage) != fmt.Sprint(want) {
		t.Fatal("byte budget refusal not told apart from a missing object", reads, coverage)
	}
}

func TestGitReaderMissingPromisorObjectDoesNotLaunchFetch(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git required")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("local inert fetch-trap fixture needs sh")
	}
	git, _ = filepath.Abs(git)
	dir := t.TempDir()
	env := []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_NO_LAZY_FETCH=0", "LC_ALL=C"}
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fixture git failed: %v: %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	run("config", "user.name", "Fixture")
	run("config", "user.email", "fixture@example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "source.go"), []byte("immutable promised object"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "source.go")
	run("commit", "-qm", "fixture")
	head, blob := run("rev-parse", "HEAD"), run("rev-parse", "HEAD:source.go")
	marker, helper := filepath.Join(dir, "fetch-invoked"), filepath.Join(dir, "inert-fetch-trap")
	quotedMarker := "'" + strings.ReplaceAll(marker, "'", "'\\''") + "'"
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf invoked > "+quotedMarker+"\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	run("config", "core.repositoryformatversion", "1")
	run("config", "extensions.partialClone", "origin")
	run("config", "remote.origin.promisor", "true")
	run("config", "remote.origin.url", "ext::"+helper)
	run("config", "protocol.ext.allow", "always")
	if err := os.Remove(filepath.Join(dir, ".git", "objects", blob[:2], blob[2:])); err != nil {
		t.Fatal(err)
	}
	// Prove the local fixture reaches the lazy-fetch helper without the control.
	// The helper only writes a marker and exits; no network command is executed.
	baseline := exec.Command(git, "cat-file", "-s", blob)
	baseline.Dir, baseline.Env = dir, env
	if baseline.Run() == nil {
		t.Fatal("missing object unexpectedly present")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("fixture did not exercise lazy fetch", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	s := testSnapshot()
	s.Head, s.Base, s.MergeBase = head, head, head
	reader, err := NewReader(GitStore{Git: git, GitDir: filepath.Join(dir, ".git")}, s, []string{"source.go"}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(context.Background(), head, "source.go"); err == nil {
		t.Fatal("missing promised object accepted")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("source read launched implicit fetch", err)
	}
	reads, coverage := reader.Evidence()
	if len(reads) != 0 || len(coverage) != 1 || coverage[0].Reason != "missing or unreadable object" {
		t.Fatal("missing promisor object not recorded as omission", reads, coverage)
	}
}
