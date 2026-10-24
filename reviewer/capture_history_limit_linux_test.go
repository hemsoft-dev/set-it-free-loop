//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// fastImport feeds stream to git fast-import in the fixture repository and
// returns the commit that the stream's last commit wrote to refs/heads/limit.
func (f snapshotGitFixture) fastImport(t *testing.T, stream []byte) string {
	t.Helper()
	c := exec.Command(f.git, "fast-import", "--quiet", "--done")
	c.Dir = f.dir
	c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	c.Stdin = bytes.NewReader(stream)
	if raw, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git fast-import: %s: %v", raw, err)
	}
	return f.run(t, "rev-parse", "refs/heads/limit")
}

// historyLimitCases are real Git histories over a default acquisition
// quarantine bound. Each new head descends from the fixture's head, so only
// the bound refuses it.
var historyLimitCases = []string{"commits over bound", "objects over bound", "objects past metadata output", "object over byte bound", "parent graph past output"}

// historyLimitFixture moves the synthetic repository's HEAD and snapshot to a
// history over one default quarantine bound.
func historyLimitFixture(t *testing.T, name string) snapshotGitFixture {
	t.Helper()
	f := newSnapshotGitFixture(t)
	s := f.snapshot
	var b bytes.Buffer
	commit := func(message string) {
		fmt.Fprintf(&b, "commit refs/heads/limit\ncommitter Synthetic <fixture@example.invalid> 1700000000 +0000\ndata %d\n%s\n", len(message), message)
	}
	files := func(count int) {
		commit("many objects")
		fmt.Fprintf(&b, "from %s\n", s.Head)
		for i := range count {
			content := fmt.Sprintf("%d\n", i)
			fmt.Fprintf(&b, "M 100644 inline many/%d\ndata %d\n%s\n", i, len(content), content)
		}
	}
	switch name {
	case "commits over bound":
		// More commits than the 8,000-commit bound, all with the head's tree.
		for i := range DefaultGitAcquisitionBounds().Commits + 1 {
			commit(fmt.Sprintf("commit %d", i))
			if i == 0 {
				fmt.Fprintf(&b, "from %s\n", s.Head)
			}
		}
	case "objects over bound":
		// More objects than the 16,000-object bound, but an inventory well
		// within the metadata output ceiling.
		files(DefaultGitAcquisitionBounds().Objects)
	case "objects past metadata output":
		// So many objects that the inventory itself is over its 2-MiB output
		// ceiling; only the exact prefix answers.
		files(50000)
	case "object over byte bound":
		commit("one large object")
		fmt.Fprintf(&b, "from %s\n", s.Head)
		size := int(DefaultGitAcquisitionBounds().ObjectBytes) + 1
		fmt.Fprintf(&b, "M 100644 inline large\ndata %d\n", size)
		b.Write(make([]byte, size))
		b.WriteString("\n")
	case "parent graph past output":
		// 200 octopus merges of the same 300 roots: few commits, but a
		// parent graph over its 2-MiB output ceiling.
		for i := range 300 {
			fmt.Fprintf(&b, "reset refs/heads/root%d\n", i)
			message := fmt.Sprintf("root %03d", i)
			fmt.Fprintf(&b, "commit refs/heads/root%d\nmark :%d\ncommitter Synthetic <fixture@example.invalid> 1700000000 +0000\ndata %d\n%s\n", i, i+1, len(message), message)
		}
		for i := range 200 {
			fmt.Fprintf(&b, "commit refs/heads/octopus%d\nmark :%d\ncommitter Synthetic <fixture@example.invalid> 1700000000 +0000\ndata %d\noctopus %d\nfrom :1\n", i, 1000+i, len(fmt.Sprintf("octopus %d", i)), i)
			for r := 2; r <= 300; r++ {
				fmt.Fprintf(&b, "merge :%d\n", r)
			}
		}
		commit("merge every octopus")
		fmt.Fprintf(&b, "from %s\n", s.Head)
		for i := range 200 {
			fmt.Fprintf(&b, "merge :%d\n", 1000+i)
		}
	default:
		t.Fatal("unknown history limit case", name)
	}
	b.WriteString("done\n")
	s.Head = f.fastImport(t, b.Bytes())
	f.run(t, "update-ref", "HEAD", s.Head)
	f.snapshot = s
	return f
}

// historyLimitMeasure independently measures the quarantine with uncapped
// Git output, so each case proves which bound it overruns.
type historyLimitMeasure struct {
	objects, commits int
	largest          int64
	metadata, graph  int
}

func measureHistoryLimit(t *testing.T, store GitStore, s Snapshot) historyLimitMeasure {
	t.Helper()
	run := func(args ...string) []byte {
		t.Helper()
		c := exec.Command(store.Git, append([]string{"--git-dir=" + store.GitDir}, args...)...)
		c.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "LC_ALL=C"}
		raw, err := c.Output()
		if err != nil {
			t.Fatal("measure", args, err)
		}
		return raw
	}
	metadata := run("cat-file", "--batch-all-objects", "--batch-check=%(objectname) %(objecttype) %(objectsize)")
	m := historyLimitMeasure{metadata: len(metadata), graph: len(run("rev-list", "--parents", s.Base, s.Head, "--"))}
	for _, line := range strings.Split(strings.TrimSpace(string(metadata)), "\n") {
		fields := strings.Fields(line)
		m.objects++
		if fields[1] == "commit" {
			m.commits++
		}
		var size int64
		fmt.Sscan(fields[2], &size)
		m.largest = max(m.largest, size)
	}
	return m
}

// Every default quarantine bound that real Git history overruns is answered
// as the permanent history_limit refusal, never as source or an opaque error.
func TestGitQuarantineDefaultBoundOverrunsRefuseHistory(t *testing.T) {
	limits := DefaultGitAcquisitionBounds()
	for _, name := range historyLimitCases {
		t.Run(name, func(t *testing.T) {
			f := historyLimitFixture(t, name)
			store, snapshot := gitQuarantineFromFixture(t, f, "sha1")
			m := measureHistoryLimit(t, store, snapshot)
			within := map[string]bool{
				"commits": m.commits <= limits.Commits, "objects": m.objects <= limits.Objects,
				"bytes": m.largest <= limits.ObjectBytes, "metadata": m.metadata <= 2<<20, "graph": m.graph <= 2<<20,
			}
			over := map[string]string{"commits over bound": "commits", "objects over bound": "objects", "objects past metadata output": "metadata", "object over byte bound": "bytes", "parent graph past output": "graph"}[name]
			if within[over] || name == "objects past metadata output" && within["objects"] {
				t.Fatal("fixture does not overrun its bound", m)
			}
			for bound, ok := range within {
				// The inventory past its output ceiling necessarily has too many
				// objects; every other case overruns exactly one bound.
				if bound != over && !ok && !(over == "metadata" && bound == "objects") {
					t.Fatal("fixture overruns another bound", bound, m)
				}
			}
			inventory, err := verifyGitQuarantine(context.Background(), store, snapshot, DefaultGitAcquisitionBounds())
			if err != (CaptureRefusal{"history_limit"}) || inventory != (GitAcquisitionInventory{}) {
				t.Fatal("history over a default bound not refused", inventory, err)
			}
			cancelled, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := verifyGitQuarantine(cancelled, store, snapshot, DefaultGitAcquisitionBounds()); !errors.Is(err, ErrGitQuarantine) {
				t.Fatal("cancelled verification reported", err)
			}
		})
	}
}

// The actual worker fetch through the broker exports each lowered object,
// commit or byte bound overrun as the exact history_limit refusal record, and
// the host decodes it to the bare refusal. The filesystem entry bound stays
// an opaque failure that exports nothing.
func TestGitAcquisitionHistoryLimitCrossesWorkerExport(t *testing.T) {
	for _, kind := range []string{"objects", "commits", "object-bytes", "expanded", "disk", "files"} {
		t.Run(kind, func(t *testing.T) {
			f := newSnapshotGitFixture(t)
			endpoint, finish := gitAcquisitionBackend(t, f)
			workspace := t.TempDir()
			if os.Chmod(workspace, 0700) != nil {
				t.Fatal("workspace")
			}
			bounds := DefaultGitAcquisitionBounds()
			switch kind {
			case "objects":
				bounds.Objects = 13
			case "commits":
				bounds.Commits = 2
			case "object-bytes":
				bounds.ObjectBytes = 64
			case "expanded":
				bounds.ExpandedBytes = 256
			case "disk":
				bounds.DiskBytes = 512
			case "files":
				bounds.Files = 4
			}
			binding := workerRefusalBinding(f.snapshot)
			var exported bytes.Buffer
			err := acquireGitMaterialization(context.Background(), &exported, f.git, workspace, endpoint, binding, bounds)
			if result := finish(); !result.audit.Stopped || result.audit.Requests == 0 {
				t.Fatal("overrun without actual fetch evidence", result.audit)
			}
			if kind == "files" {
				if !errors.Is(err, ErrGitQuarantine) || exported.Len() != 0 {
					t.Fatal("filesystem entry overrun exported", err, exported.Len())
				}
				return
			}
			if err != nil || !bytes.Equal(exported.Bytes(), gitCaptureRefusalWireFixture(t, binding, "history_limit")) {
				t.Fatalf("overrun not exported as the exact refusal record: %v %q", err, exported.Bytes())
			}
			m, err := receiveGitMaterialization(context.Background(), io.NopCloser(bytes.NewReader(exported.Bytes())), binding)
			if m != nil || err != (CaptureRefusal{"history_limit"}) {
				t.Fatal("host did not decode the unwrapped refusal", err)
			}
		})
	}
}

// The host decodes history_limit only as the exact canonical record for its
// own attempt, inside its own VM envelope. Near-miss reasons, foreign
// bindings, data after the record and every malformed shape stay opaque.
func TestGitMaterializationReceiverDecodesHistoryLimitStrictly(t *testing.T) {
	_, binding, _ := gitMaterializationFixture(t, "sha1")
	valid := gitCaptureRefusalWireFixture(t, binding, "history_limit")
	if m, err := receiveGitMaterialization(context.Background(), io.NopCloser(bytes.NewReader(valid)), binding); m != nil || err != (CaptureRefusal{"history_limit"}) {
		t.Fatal("exact history_limit record not accepted", err)
	}
	canonicalBinding, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	record := func(fields string) []byte {
		return gitCaptureRefusalFrame("SFL-GIT-CAPTURE-REFUSAL-1\n", []byte(fields))
	}
	other := binding
	other.Attempt = strings.Repeat("b", 32)
	moved := binding
	moved.Snapshot.Head = moved.Snapshot.Base
	runtime := binding
	runtime.RuntimeHash = strings.Repeat("4", 64)
	for name, test := range map[string]struct {
		wire     []byte
		expected gitMaterializationBinding
	}{
		"cased reason":      {wire: gitCaptureRefusalWireFixture(t, binding, "History_Limit")},
		"dashed reason":     {wire: gitCaptureRefusalWireFixture(t, binding, "history-limit")},
		"plural reason":     {wire: gitCaptureRefusalWireFixture(t, binding, "history_limits")},
		"padded reason":     {wire: gitCaptureRefusalWireFixture(t, binding, " history_limit")},
		"escaped reason":    {wire: record(`{"Version":1,"Binding":` + string(canonicalBinding) + `,"Reason":"history\u005flimit"}`)},
		"duplicate reason":  {wire: record(`{"Version":1,"Binding":` + string(canonicalBinding) + `,"Reason":"inventory_limit","Reason":"history_limit"}`)},
		"bound counts":      {wire: record(`{"Version":1,"Binding":` + string(canonicalBinding) + `,"Reason":"history_limit","Objects":16001}`)},
		"version":           {wire: record(`{"Version":2,"Binding":` + string(canonicalBinding) + `,"Reason":"history_limit"}`)},
		"other attempt":     {wire: gitCaptureRefusalWireFixture(t, other, "history_limit")},
		"other snapshot":    {wire: gitCaptureRefusalWireFixture(t, moved, "history_limit")},
		"other runtime":     {wire: gitCaptureRefusalWireFixture(t, runtime, "history_limit")},
		"another receiver":  {wire: valid, expected: other},
		"trailing byte":     {wire: append(append([]byte(nil), valid...), '\n')},
		"truncated":         {wire: valid[:len(valid)-2]},
		"refusal as source": {wire: append([]byte(gitMaterializationMagic), valid[len(gitMaterializationMagic):]...)},
		"second refusal":    {wire: append(append([]byte(nil), valid...), valid...)},
		"foreign marker":    {wire: gitCaptureRefusalFrame("SFL-GIT-CAPTURE-REFUSAL-2\n", valid[len(gitMaterializationMagic)+4:])},
		"lowercase marker":  {wire: gitCaptureRefusalFrame("sfl-git-capture-refusal-1\n", valid[len(gitMaterializationMagic)+4:])},
	} {
		t.Run(name, func(t *testing.T) {
			expected := binding
			if test.expected.Attempt != "" {
				expected = test.expected
			}
			m, err := receiveGitMaterialization(context.Background(), io.NopCloser(bytes.NewReader(test.wire)), expected)
			if m != nil {
				m.Close()
			}
			if m != nil || errors.As(err, new(CaptureRefusal)) || !errors.Is(err, errGitMaterialization) {
				t.Fatal("malformed or foreign history_limit record accepted", err)
			}
		})
	}
	q := gitVMRequestFixture(t, binding)
	var wire bytes.Buffer
	if writeGitVMRefusal(context.Background(), &wire, q, "history_limit") != nil {
		t.Fatal("guest refusal writer")
	}
	if m, err := receiveGitVMExport(context.Background(), io.NopCloser(bytes.NewReader(wire.Bytes())), q); m != nil || err != (CaptureRefusal{"history_limit"}) {
		t.Fatal("VM history_limit refusal not decoded", err)
	}
	foreign := gitVMRequestFixture(t, other)
	if m, err := receiveGitVMExport(context.Background(), io.NopCloser(bytes.NewReader(wire.Bytes())), foreign); m != nil || errors.As(err, new(CaptureRefusal)) {
		t.Fatal("VM history_limit refusal accepted for another request", err)
	}
	if m, err := receiveGitVMExport(context.Background(), io.NopCloser(bytes.NewReader(append(wire.Bytes(), 'x'))), q); m != nil || errors.As(err, new(CaptureRefusal)) {
		t.Fatal("VM history_limit refusal with data accepted", err)
	}
}

// The metadata parser answers an overrun only from complete, well-formed
// lines. A malformed or duplicate line, even after an overrun, and a
// truncated inventory without one are opaque.
func TestGitQuarantineMetadataOverrunNeedsWellFormedLines(t *testing.T) {
	s := testSnapshot()
	bounds := GitAcquisitionBounds{Objects: 2, Commits: 1, Files: 1, ObjectBytes: 100, ExpandedBytes: 150, DiskBytes: 1}
	line := func(i int, kind string, size int) string {
		return fmt.Sprintf("%040x %s %d\n", i+1, kind, size)
	}
	for name, test := range map[string]struct {
		raw       string
		truncated bool
		over      bool
		ok        bool
	}{
		"within":               {raw: line(0, "commit", 10) + line(1, "tree", 10), ok: true},
		"objects":              {raw: line(0, "commit", 1) + line(1, "tree", 1) + line(2, "blob", 1), over: true, ok: true},
		"commits":              {raw: line(0, "commit", 1) + line(1, "commit", 1), over: true, ok: true},
		"object bytes":         {raw: line(0, "blob", 101), over: true, ok: true},
		"expanded":             {raw: line(0, "blob", 100) + line(1, "blob", 51), over: true, ok: true},
		"truncated overrun":    {raw: line(0, "commit", 1) + line(1, "tree", 1) + line(2, "blob", 1) + "0000", truncated: true, over: true, ok: true},
		"truncated within":     {raw: line(0, "commit", 1) + "0000", truncated: true},
		"truncated no line":    {raw: "0000", truncated: true},
		"malformed after over": {raw: line(0, "commit", 1) + line(1, "commit", 1) + "not an object line\n"},
		"duplicate after over": {raw: line(0, "commit", 1) + line(1, "commit", 1) + line(1, "commit", 1)},
		"unknown type":         {raw: line(0, "commit", 1) + line(1, "commit", 1) + line(2, "note", 1)},
		"negative size":        {raw: line(0, "blob", -1)},
		"empty":                {raw: ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, over, err := parseGitQuarantineMetadata([]byte(test.raw), test.truncated, s, bounds)
			if (err == nil) != test.ok || over != test.over {
				t.Fatal("metadata overrun misclassified", over, err)
			}
		})
	}
}

// Over a fake upstream and through each production source factory, an older
// PR whose history overruns a quarantine bound is refused once with one
// INCOMPLETE status, and a later PR is then admitted and reviewed.
func TestPollingHistoryLimitRefusalDoesNotBlockOtherPRs(t *testing.T) {
	for _, kind := range []string{"host", "source VM"} {
		t.Run(kind, func(t *testing.T) {
			limit, store := refusedWorkerStore(t, "history over commit bound")
			good := newSnapshotGitFixture(t)
			var factoryCapture func(context.Context, Snapshot) (*SnapshotEvidence, error)
			if kind == "host" {
				factory := refusalSourceFactoryFixture(t)
				factoryCapture = func(ctx context.Context, s Snapshot) (*SnapshotEvidence, error) {
					return factory.capture(ctx, s, refusingHostCapture(store, nil))
				}
			} else {
				factory := refusalVMSourceFactoryFixture(t)
				factoryCapture = func(ctx context.Context, s Snapshot) (*SnapshotEvidence, error) {
					return factory.capture(ctx, s, refusingVMCapture(store, nil))
				}
			}
			var mu sync.Mutex
			captured := map[int]int{}
			c, durable, u, clock, calls := twoPRPolling(t, limit.snapshot, good.snapshot, func(ctx context.Context, s Snapshot) (*SnapshotEvidence, error) {
				mu.Lock()
				captured[s.PR]++
				mu.Unlock()
				if s.PR == 1 {
					return factoryCapture(ctx, s)
				}
				return CaptureSnapshotEvidence(ctx, good.store(), s)
			}, func(ctx context.Context, job Job) (*JobEvidence, error) {
				return RestoreSnapshotEvidence(ctx, good.store(), job)
			})
			// Each cycle passes any deferred backoff, so only selection and
			// the refusal decide progress.
			for range 6 {
				result, _ := pollOnce(t, c)
				if result.Active {
					awaitPollWorker(t, c)
				}
				clock.Advance(2 * time.Minute)
			}
			d, err := durable.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			states := map[int]*DurableJob{}
			for _, j := range d.Jobs {
				states[j.Scope.PR] = j
			}
			mu.Lock()
			defer mu.Unlock()
			if states[2] == nil || states[2].State != "dual_complete" || calls.Load() != 4 || captured[2] != 1 {
				t.Fatal("overrunning older PR blocked a later PR", states[2], calls.Load(), captured)
			}
			if states[1] == nil || states[1].State != "capture_refused" || refusalPacketReason(t, states[1].Job) != "history_limit" || captured[1] != 1 {
				t.Fatal("overrunning PR not refused once", states[1], captured)
			}
			if d.Polling == nil || d.Polling.Failures != 0 {
				t.Fatal("refusal left a deferred backoff", d.Polling)
			}
			u.mu.Lock()
			defer u.mu.Unlock()
			kinds := map[int]string{}
			for _, w := range u.posts {
				kinds[w.Scope.PR] += w.Kind + ":" + w.Status + " "
				if w.Scope.PR == 1 && (!strings.Contains(w.Document.Summary, historyLimitCause) || strings.Contains(w.Document.Summary, "No findings reported")) {
					t.Fatal("refusal status does not name history_limit or reads as clean", w.Document.Summary)
				}
			}
			if len(u.posts) != 2 || kinds[1] != "status:capture_refused " || kinds[2] != "review: " {
				t.Fatal("wrong publications", kinds)
			}
		})
	}
}
