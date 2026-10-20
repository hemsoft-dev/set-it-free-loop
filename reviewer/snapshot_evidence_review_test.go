package reviewer

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

func TestSnapshotReviewSubmoduleIgnoreCannotHideChangedGitlink(t *testing.T) {
	f := newSnapshotGitFixture(t)
	old := f.snapshot.Head
	f.write(t, ".gitmodules", "[submodule \"submodule\"]\n\tpath = submodule\n\turl = https://example.invalid/inert\n\tignore = all\n")
	f.run(t, "config", "submodule.submodule.ignore", "all")
	f.run(t, "config", "diff.ignoreSubmodules", "all")
	f.run(t, "add", ".gitmodules")
	f.run(t, "update-index", "--cacheinfo", "160000,"+old+",submodule")
	f.run(t, "commit", "--quiet", "-m", "changed ignored synthetic gitlink")
	f.snapshot.Head = f.run(t, "rev-parse", "HEAD")
	f.snapshot.Base = old
	f.snapshot.MergeBase = old
	if baseline := f.run(t, "diff", "--raw", "--no-renames", old, f.snapshot.Head, "--", "submodule"); baseline != "" {
		t.Fatal("fixture did not hide gitlink without override", baseline)
	}
	e, err := CaptureSnapshotEvidence(context.Background(), f.store(), f.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range e.Coverage() {
		if c.Path == "submodule" && strings.Contains(c.Reason, "submodule") {
			found = true
		}
	}
	if !found {
		t.Fatal("ignored submodule absent from authoritative inventory", e.Coverage())
	}
}

func TestSnapshotReviewSearchContinuesAfterOmittedPaths(t *testing.T) {
	f := newSnapshotGitFixture(t)
	e, err := CaptureSnapshotEvidence(context.Background(), f.store(), f.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	r, err := bindSnapshotEvidence(t, e).Reader(f.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	matches, err := r.Search(context.Background(), f.snapshot.Head, "FOUR", 1)
	if err != nil || len(matches) != 1 || matches[0].Path != "safe.go" || matches[0].Line != 4 {
		t.Fatal("omissions disabled later text search", matches, err)
	}
	_, coverage := r.Evidence()
	if len(coverage) < 3 {
		t.Fatal("search erased omissions", coverage)
	}
	if _, err := r.Search(context.Background(), strings.Repeat("f", 40), "FOUR", 1); err == nil {
		t.Fatal("search accepted foreign revision")
	}
}

func TestSnapshotReviewSourceCoverageLeavesRoomForExhaustion(t *testing.T) {
	files := map[string]Object{}
	paths := []string{}
	initial := []Coverage{}
	for i := 0; i < 128; i++ {
		path := "binary-" + strconv.Itoa(i)
		files[path] = Object{"100644", []byte{0}}
		paths = append(paths, path)
		initial = append(initial, Coverage{path, "controller binary omission"})
	}
	r, err := NewReader(&fixtureStore{files: files}, testSnapshot(), paths, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	r.initialCoverage = initial
	r.coverage = append([]Coverage{}, initial...)
	session, err := NewSession("coverage-test", testSnapshot(), Digest("config"), Codex, 1, "captured inventory", nil, nil, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	broker, err := newSourceBroker(context.Background(), session, r)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if _, err := broker.Read(context.Background(), session.Snapshot.Head, path); err == nil {
			t.Fatal("binary read succeeded")
		}
	}
	if _, err := broker.Read(context.Background(), session.Snapshot.Head, paths[0]); err == nil {
		t.Fatal("tool budget not enforced")
	}
	a := broker.seal()
	if err := a.validate(session); err != nil {
		t.Fatal("ordinary exhaustion invalidated source receipt", len(a.Coverage), err)
	}
	seen := map[string]bool{}
	exhaustion := false
	for _, c := range a.Coverage {
		if c.Path != "" {
			seen[c.Path] = true
		}
		if strings.Contains(c.Reason, "tool-call limit exhausted") {
			exhaustion = true
		}
	}
	if len(seen) != 128 || !exhaustion || !a.Exhausted {
		t.Fatal("coverage lost inventory or exhaustion", len(seen), exhaustion)
	}
}
