package reviewer

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type snapshotGitFixture struct {
	dir, git string
	snapshot Snapshot
}

func (f snapshotGitFixture) run(t *testing.T, args ...string) string {
	t.Helper()
	c := exec.Command(f.git, args...)
	c.Dir = f.dir
	c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	b, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s: %v", args, b, err)
	}
	return strings.TrimSpace(string(b))
}
func (f snapshotGitFixture) write(t *testing.T, path, data string) {
	t.Helper()
	p := filepath.Join(f.dir, path)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
func (f snapshotGitFixture) store() GitStore { return GitStore{f.git, filepath.Join(f.dir, ".git")} }

func newSnapshotGitFixture(t *testing.T) snapshotGitFixture {
	return snapshotGitFixtureFormat(t, "sha1")
}

func snapshotGitFixtureFormat(t *testing.T, format string) snapshotGitFixture {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git required")
	}
	git, err = filepath.Abs(git)
	if err != nil {
		t.Fatal(err)
	}
	f := snapshotGitFixture{dir: t.TempDir(), git: git}
	f.run(t, "init", "--quiet", "--object-format="+format)
	f.run(t, "config", "user.name", "Synthetic fixture")
	f.run(t, "config", "user.email", "fixture@example.invalid")
	f.write(t, "safe.go", "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\neleven\ntwelve\nthirteen\nfourteen\nfifteen\nsixteen\nseventeen\neighteen\nnineteen\ntwenty\n")
	f.write(t, "removed.go", "removed without newline")
	f.write(t, "renamed.go", "same file\n")
	f.run(t, "add", "--all")
	f.run(t, "commit", "--quiet", "-m", "old synthetic tree")
	old := f.run(t, "rev-parse", "HEAD")
	f.write(t, "safe.go", strings.ReplaceAll(strings.ReplaceAll(string(mustSnapshotRead(t, filepath.Join(f.dir, "safe.go"))), "four\n", "FOUR\n"), "seventeen\n", "SEVENTEEN\n"))
	if err := os.Remove(filepath.Join(f.dir, "removed.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(f.dir, "renamed.go"), filepath.Join(f.dir, "new name.go")); err != nil {
		t.Fatal(err)
	}
	f.write(t, "dir/sp ace[1]\".go", "@@ -0,0 +1 @@\n+++ content\nΔ without final newline")
	f.write(t, "binary", "private synthetic\x00bytes")
	if err := os.Symlink("/synthetic-unreadable-canary", filepath.Join(f.dir, "link")); err != nil {
		t.Fatal(err)
	}
	f.write(t, ".gitattributes", "*.go diff=hostile\n")
	f.run(t, "add", "--all")
	f.run(t, "update-index", "--add", "--cacheinfo", "160000,"+old+",submodule")
	f.run(t, "commit", "--quiet", "-m", "new synthetic tree")
	head := f.run(t, "rev-parse", "HEAD")
	// A sibling base commit with the same tree as head makes base-tip-to-head
	// empty. The intended merge-base-to-head comparison must still find edits.
	base := f.run(t, "commit-tree", f.run(t, "rev-parse", "HEAD^{tree}"), "-p", old, "-m", "independent base advance")
	f.snapshot = Snapshot{RepositoryID: RepositoryID, HeadRepositoryID: 5555, PR: 1, Head: head, Base: base, MergeBase: old}
	return f
}
func mustSnapshotRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSnapshotEvidenceUsesActualGitWithoutCheckoutOrExternalDrivers(t *testing.T) {
	f := newSnapshotGitFixture(t)
	canary := filepath.Join(f.dir, "EXECUTED")
	driver := filepath.Join(f.dir, ".git", "forbidden-driver")
	if err := os.WriteFile(driver, []byte("#!/bin/sh\ntouch '"+canary+"'\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	f.run(t, "config", "diff.hostile.command", driver)
	f.run(t, "config", "diff.hostile.textconv", driver)
	if err := os.WriteFile(filepath.Join(f.dir, ".git/hooks/post-checkout"), mustSnapshotRead(t, driver), 0700); err != nil {
		t.Fatal(err)
	}
	f.write(t, "safe.go", "working tree canary must not enter packet\n")
	t.Setenv("GIT_EXTERNAL_DIFF", driver)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "diff.external")
	t.Setenv("GIT_CONFIG_VALUE_0", driver)
	e, err := CaptureSnapshotEvidence(context.Background(), f.store(), f.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(canary); !os.IsNotExist(err) {
		t.Fatal("candidate hook/driver executed", err)
	}
	if strings.Contains(e.Packet(), "working tree canary") {
		t.Fatal("working tree read")
	}
	var p snapshotPacket
	if err := json.Unmarshal([]byte(e.Packet()), &p); err != nil {
		t.Fatal(err)
	}
	if p.Snapshot != f.snapshot || p.Version != "sfl-snapshot-evidence-1" || len(p.Files) != 9 || len(p.Coverage) != 3 {
		t.Fatalf("%+v", p)
	}
	bound := bindSnapshotEvidence(t, e)
	lines := bound.diffLines()
	if !lines["safe.go"][4] || !lines["safe.go"][17] || lines["safe.go"][10] || len(lines["removed.go"]) != 0 || !lines["dir/sp ace[1]\".go"][3] {
		t.Fatal(lines)
	}
	if len(lines["binary"]) != 0 || len(lines["link"]) != 0 || len(lines["submodule"]) != 0 {
		t.Fatal("omitted object anchors", lines)
	}
	for _, file := range p.Files {
		if file.Path == "new name.go" && file.Status != "A" {
			t.Fatal("rename pairing was not disabled")
		}
	}
	// Accessors cannot mutate the capture used later for admission/publication.
	lines["safe.go"][10] = true
	paths := e.Paths()
	paths[0] = "outside"
	coverage := e.Coverage()
	coverage[0].Reason = "forged"
	if bindSnapshotEvidence(t, e).diffLines()["safe.go"][10] || e.Paths()[0] == "outside" || e.Coverage()[0].Reason == "forged" {
		t.Fatal("mutable evidence")
	}
	r, err := bound.Reader(f.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	text, err := r.Read(context.Background(), f.snapshot.Head, "safe.go")
	if err != nil || !strings.Contains(text, "FOUR\n") {
		t.Fatal(text, err)
	}
	_, c := r.Evidence()
	if len(c) != 3 {
		t.Fatal(c)
	}
	s, err := NewSession("fixture", f.snapshot, Digest("config"), Codex, 1, e.Packet(), nil, nil, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	b, err := newSourceBroker(context.Background(), s, r)
	if err != nil {
		t.Fatal(err)
	}
	_, c = b.reader.Evidence()
	if len(c) != 3 {
		t.Fatal("fork lost authoritative omissions", c)
	}
	if b.reader.calls != 0 || b.reader.used != 0 {
		t.Fatal("fork inherited citation budget")
	}
	wrong := f.snapshot
	wrong.HeadRepositoryID++
	if _, err := bound.Reader(wrong); err != ErrFenced {
		t.Fatal("snapshot mismatch", err)
	}
}

func TestSnapshotEvidenceRefusesMissingOrAmbiguousHistory(t *testing.T) {
	f := newSnapshotGitFixture(t)
	for _, field := range []string{"head", "base", "merge-base", "not-commit", "incorrect-merge-base"} {
		t.Run(field, func(t *testing.T) {
			s := f.snapshot
			switch field {
			case "head":
				s.Head = strings.Repeat("f", 40)
			case "base":
				s.Base = strings.Repeat("f", 40)
			case "merge-base":
				s.MergeBase = strings.Repeat("f", 40)
			case "not-commit":
				s.Head = f.run(t, "rev-parse", "HEAD:safe.go")
			case "incorrect-merge-base":
				s.MergeBase = s.Head
			}
			if e, err := CaptureSnapshotEvidence(context.Background(), f.store(), s); err == nil || e != nil {
				t.Fatal(e, err)
			}
		})
	}
	s := f.snapshot
	s.RepositoryID++
	if _, err := CaptureSnapshotEvidence(context.Background(), GitStore{}, s); err != ErrIgnored {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e, err := CaptureSnapshotEvidence(ctx, f.store(), f.snapshot); err == nil || e != nil {
		t.Fatal(e, err)
	}
}

func TestSnapshotEvidenceRetainsSizeOmissionsAndEnforcesInventoryCap(t *testing.T) {
	f := newSnapshotGitFixture(t)
	f.write(t, "huge.go", strings.Repeat("x", snapshotObjectBytes+1))
	f.write(t, "patch-large.go", strings.Repeat("unique line\n", 8000))
	f.run(t, "add", "--all")
	f.run(t, "commit", "--quiet", "-m", "large synthetic sources")
	f.snapshot.Head = f.run(t, "rev-parse", "HEAD")
	e, err := CaptureSnapshotEvidence(context.Background(), f.store(), f.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{}
	for _, c := range e.Coverage() {
		reasons[c.Path] = c.Reason
	}
	if !strings.Contains(reasons["huge.go"], "source byte limit") || !strings.Contains(reasons["patch-large.go"], "packet byte limit") || len(e.Packet()) > snapshotPacketBytes {
		t.Fatal(reasons)
	}
	if len(bindSnapshotEvidence(t, e).diffLines()["huge.go"]) != 0 || len(bindSnapshotEvidence(t, e).diffLines()["patch-large.go"]) != 0 {
		t.Fatal("omitted patch admitted inline")
	}
	for i := 0; i <= snapshotFileLimit; i++ {
		f.write(t, "many/"+strconv.Itoa(i), "x\n")
	}
	f.run(t, "add", "--all")
	f.run(t, "commit", "--quiet", "-m", "too many paths")
	f.snapshot.Head = f.run(t, "rev-parse", "HEAD")
	if e, err := CaptureSnapshotEvidence(context.Background(), f.store(), f.snapshot); err == nil || e != nil {
		t.Fatal("partial inventory admitted", e, err)
	}
}

func TestSnapshotHunksRejectForgedCoordinatesTextAndMissingEdits(t *testing.T) {
	valid := "diff --git a b\n--- a\n+++ b\n@@ -1,2 +1,2 @@ arbitrary metadata\n one\n-two\n+TWO\n"
	lines, err := snapshotHunkLines(valid, "one\ntwo\n", "one\nTWO\n")
	if err != nil || !lines[2] {
		t.Fatal(lines, err)
	}
	for name, patch := range map[string]string{
		"wrong source":            strings.Replace(valid, "+TWO", "+forged", 1),
		"wrong deleted source":    strings.Replace(valid, "-two", "-forged", 1),
		"wrong count":             strings.Replace(valid, "-1,2", "-1,3", 1),
		"out of bounds":           strings.Replace(valid, "+1,2", "+999999999999999999999999,2", 1),
		"zero start with content": strings.Replace(valid, "+1,2", "+0,2", 1),
		"unfinished":              strings.TrimSuffix(valid, "+TWO\n"),
		"extra content":           valid + "+extra\n",
		"overlap":                 valid + "@@ -1,2 +1,2 @@\n one\n-two\n+TWO\n",
		"missing edits":           "diff --git a b\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := snapshotHunkLines(patch, "one\ntwo\n", "one\nTWO\n"); err == nil {
				t.Fatal("forged patch accepted")
			}
		})
	}
	if _, err := snapshotHunkLines("@@ -3 +3 @@\n c\n", "one\nb\nc\n", "TWO\nb\nc\n"); err == nil {
		t.Fatal("hidden edit in unchanged gap")
	}
}

func TestSnapshotRawChangesRejectUnsafeAndIncompleteInventories(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	valid := ":100644 100644 " + a + " " + b + " M\x00safe.go\x00"
	for _, raw := range []string{strings.TrimSuffix(valid, "\x00"), valid + valid, strings.Replace(valid, "safe.go", "../escape", 1), strings.Replace(valid, " M", " R100", 1), strings.Replace(valid, a, strings.Repeat("0", 40), 1), strings.Replace(valid, "100644", "000000", 1), strings.Replace(valid, a, "bad", 1)} {
		if _, err := parseSnapshotChanges([]byte(raw), 40); err == nil {
			t.Fatal("malformed raw accepted")
		}
	}
}

func TestSnapshotHunksVerifyEndOfFileNewlineMarkers(t *testing.T) {
	patch := "@@ -1 +1 @@\n-old\n\\ No newline at end of file\n+new\n\\ No newline at end of file\n"
	if lines, err := snapshotHunkLines(patch, "old", "new"); err != nil || !lines[1] {
		t.Fatal(lines, err)
	}
	for _, p := range []string{strings.Replace(patch, "\\ No newline at end of file\n", "", 1), strings.TrimSuffix(patch, "\\ No newline at end of file\n"), "@@ -1 +1 @@\n-old\n+new\n\\ No newline at end of file\n"} {
		if _, err := snapshotHunkLines(p, "old", "new"); err == nil {
			t.Fatal("missing EOF marker accepted")
		}
	}
	if _, err := snapshotHunkLines(patch, "old\n", "new\n"); err == nil {
		t.Fatal("forged EOF marker accepted")
	}
	if _, err := snapshotHunkLines("diff --git a b\n", "old\n", "old"); err == nil {
		t.Fatal("hidden newline edit accepted")
	}
}

func TestSnapshotEvidenceOmissionsSurviveDurableFakeReviewAndRecovery(t *testing.T) {
	f := newSnapshotGitFixture(t)
	evidence, err := CaptureSnapshotEvidence(context.Background(), f.store(), f.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	store, _ := openFixtureStore(t)
	request := native(1, controlTime)
	request.Snapshot = f.snapshot
	if err := store.Discover(context.Background(), Discovery{Scope: alphaScope(), Requests: []NativeRequest{request}, Through: controlTime.Add(time.Minute), Complete: true}); err != nil {
		t.Fatal(err)
	}
	job, err := store.Admit(context.Background(), alphaScope(), 1, strings.Repeat("d", 40), evidence.Packet(), controlTime.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	fake := reviewFunc(func(_ context.Context, session Session) (Report, error) {
		calls++
		// No model coverage declaration. Controller observations must survive.
		return fakeReport(session), nil
	})
	bound, err := evidence.Bind(job)
	if err != nil {
		t.Fatal(err)
	}
	engine := Engine{Reviewers: map[Provider]Reviewer{Codex: fake, Claude: fake}, Reader: func(s Snapshot) (*Reader, error) { return bound.Reader(s) }}
	out, err := store.RunStopped(context.Background(), job.ID, engine, func() time.Time { return controlTime.Add(3 * time.Minute) })
	if err != nil || out.State != "dual_complete" || calls != 4 {
		t.Fatal(out.State, calls, err)
	}
	for _, artifact := range out.Artifacts {
		if len(artifact.Report.Coverage) != 3 {
			t.Fatal("model erased authoritative coverage", artifact.Report.Coverage)
		}
	}
	recovered, err := store.RecoverCompleted(context.Background(), job.ID, engine.Reader)
	if err != nil || calls != 4 {
		t.Fatal("recovery repeated fake sessions", calls, err)
	}
	doc, err := bound.Render(recovered)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"binary", "link", "submodule"} {
		if !strings.Contains(doc.Summary, path) {
			t.Fatal("omission absent from publication", path)
		}
	}
}

func bindSnapshotEvidence(t *testing.T, e *SnapshotEvidence) *JobEvidence {
	t.Helper()
	job := testJob()
	job.Snapshot = e.snapshot
	job.Packet = e.Packet()
	bound, err := e.Bind(job)
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

func TestSnapshotEvidenceBindingRejectsRecapturedOrSubstitutedJob(t *testing.T) {
	f := newSnapshotGitFixture(t)
	e, err := CaptureSnapshotEvidence(context.Background(), f.store(), f.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	job := testJob()
	job.Snapshot = f.snapshot
	job.Packet = e.Packet()
	for _, change := range []func(*Job){func(j *Job) { j.Packet += " " }, func(j *Job) { j.Snapshot.HeadRepositoryID++ }, func(j *Job) { j.RequesterID++ }, func(j *Job) { j.Config.Limits.ReaderCalls = 0 }} {
		bad := job
		change(&bad)
		if _, err := e.Bind(bad); err != ErrFenced {
			t.Fatal("mismatched evidence accepted", err)
		}
	}
}

func TestSnapshotEvidenceTypedPublicationRejectsDifferentAdmittedJob(t *testing.T) {
	f := newSnapshotGitFixture(t)
	e, err := CaptureSnapshotEvidence(context.Background(), f.store(), f.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	job := testJob()
	job.Snapshot = f.snapshot
	job.Packet = e.Packet()
	bound, err := e.Bind(job)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Render(Outcome{Job: job, State: "provider_unavailable"}, bound.diffLines()); err != ErrFenced {
		t.Fatal("captured packet accepted unbound render map", err)
	}
	if _, err := (Publisher{}).Prepare(context.Background(), Outcome{Job: job}, bound.diffLines()); err != ErrFenced {
		t.Fatal("captured packet accepted unbound publication map", err)
	}
	for _, change := range []func(*Job){func(j *Job) { j.ID = "different-job" }, func(j *Job) { j.Packet += " " }, func(j *Job) { j.Generation++ }, func(j *Job) { j.Snapshot.HeadRepositoryID++ }} {
		other := job
		change(&other)
		out := Outcome{Job: other, State: "provider_unavailable"}
		if _, err := bound.Render(out); err != ErrFenced {
			t.Fatal("cross-job render accepted", err)
		}
		// A nil publisher would panic if the binding check reached storage.
		if _, err := (Publisher{}).PrepareEvidence(context.Background(), out, bound); err != ErrFenced {
			t.Fatal("cross-job publication reached storage", err)
		}
	}
}

func TestSnapshotEvidenceReadsActualSHA256GitObjects(t *testing.T) {
	f := snapshotGitFixtureFormat(t, "sha256")
	e, err := CaptureSnapshotEvidence(context.Background(), f.store(), f.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.snapshot.Head) != 64 || !bindSnapshotEvidence(t, e).diffLines()["safe.go"][4] {
		t.Fatal("SHA256 object protocol not captured")
	}
}

func TestSnapshotEvidenceMissingPromisorBlobNeverFetches(t *testing.T) {
	f := newSnapshotGitFixture(t)
	marker, helper := filepath.Join(f.dir, "FETCHED"), filepath.Join(f.dir, ".git", "fetch-trap")
	quoted := "'" + strings.ReplaceAll(marker, "'", "'\\''") + "'"
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf invoked > "+quoted+"\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	blob := f.run(t, "rev-parse", "HEAD:safe.go")
	f.run(t, "config", "extensions.partialClone", "origin")
	f.run(t, "config", "remote.origin.promisor", "true")
	f.run(t, "config", "remote.origin.url", "ext::"+helper)
	f.run(t, "config", "protocol.ext.allow", "always")
	if err := os.Remove(filepath.Join(f.dir, ".git", "objects", blob[:2], blob[2:])); err != nil {
		t.Fatal(err)
	}
	// A local inert trap proves this setup actually attempts a lazy fetch
	// without the production command's no-fetch controls. No network is used.
	c := exec.Command(f.git, "cat-file", "-s", blob)
	c.Dir = f.dir
	c.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_NO_LAZY_FETCH=0", "LC_ALL=C"}
	if c.Run() == nil {
		t.Fatal("missing blob unexpectedly readable")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("inert fetch trap not reached", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	e, err := CaptureSnapshotEvidence(context.Background(), f.store(), f.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("implicit fetch executed", err)
	}
	found := false
	for _, coverage := range e.Coverage() {
		if coverage.Path == "safe.go" && strings.Contains(coverage.Reason, "new source unavailable") {
			found = true
		}
	}
	if !found || len(bindSnapshotEvidence(t, e).diffLines()["safe.go"]) != 0 {
		t.Fatal("missing source counted as clean", e.Coverage())
	}
}
