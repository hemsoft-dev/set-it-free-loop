package reviewer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const frozenCorpusHash = "807196990ee0adda9c8a2f87b0c9b6a5819baf886fa4196b58f6b102d7ea4910"

func retainedFrozenCorpus(t *testing.T) EvaluationCorpus {
	t.Helper()
	raw, err := os.ReadFile("testdata/evaluation/frozen/corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	c, err := DecodeStrict[EvaluationCorpus](raw, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Validate(); err != nil {
		t.Fatal(err)
	}
	digest, err := os.ReadFile("testdata/evaluation/frozen/corpus.sha256")
	if err != nil || string(digest) != frozenCorpusHash+"\n" || c.Hash() != frozenCorpusHash {
		t.Fatal("frozen corpus digest differs", err)
	}
	return c
}

func TestFrozenEvaluationPreservesOriginalLabelsAndExplicitSelection(t *testing.T) {
	c := retainedFrozenCorpus(t)
	raw, err := os.ReadFile("testdata/evaluation/labels.json")
	if err != nil {
		t.Fatal(err)
	}
	original, err := DecodeStrict[EvaluationCorpus](raw, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	old := map[int]EvaluationCase{}
	for _, test := range original.Cases {
		old[test.Snapshot.PR] = test
	}
	clean, defects, retained, added := 0, 0, 0, 0
	for _, test := range c.Cases {
		if test.Snapshot.PR == 156 {
			t.Fatal("oversized case silently entered benchmark")
		}
		if prior, ok := old[test.Snapshot.PR]; ok {
			if prior.Snapshot != test.Snapshot || Digest(prior.Label) != Digest(test.Label) || Digest(prior.LabelHistory) != Digest(test.LabelHistory) || !prior.MergedAt.Equal(test.MergedAt) {
				t.Fatal("original label/snapshot changed", test.ID)
			}
			retained++
		} else if test.Snapshot.PR == 148 {
			raw, err := os.ReadFile("testdata/evaluation/replacement-148.json")
			if err != nil {
				t.Fatal(err)
			}
			label, err := DecodeStrict[EvaluationCase](raw, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			if label.Snapshot != test.Snapshot || Digest(label.Label) != Digest(test.Label) {
				t.Fatal("replacement label drift")
			}
			added++
		} else {
			t.Fatal("undeclared added case", test.ID)
		}
		packet, err := os.ReadFile(filepath.Join("testdata/evaluation/frozen", test.ID+".packet.json"))
		if err != nil {
			t.Fatal(err)
		}
		if len(packet) != test.SerializedPacketBytes || Digest(string(packet)) != test.PacketHash {
			t.Fatal("retained packet differs", test.ID)
		}
		if test.Label.Clean {
			clean++
		} else {
			defects++
		}
	}
	if len(c.Cases) != 20 || clean != 14 || defects != 6 || retained != 19 || added != 1 || !old[156].Label.Clean {
		t.Fatal("selection/counts changed")
	}
	raw, err = os.ReadFile("testdata/evaluation/selection.json")
	if err != nil {
		t.Fatal(err)
	}
	var selection struct {
		CorpusHash  string
		SelectedPRs []int
		Excluded    []struct {
			PR     int
			Reason string
		}
		Added         []struct{ PR int }
		PlanFrozen    bool
		BenchmarkRuns int
	}
	if err = json.Unmarshal(raw, &selection); err != nil {
		t.Fatal(err)
	}
	ids := []int{}
	for _, test := range c.Cases {
		ids = append(ids, test.Snapshot.PR)
	}
	if selection.CorpusHash != c.Hash() || !reflect.DeepEqual(ids, selection.SelectedPRs) || len(selection.Excluded) != 1 || selection.Excluded[0].PR != 156 || !strings.Contains(selection.Excluded[0].Reason, "128") || len(selection.Added) != 1 || selection.Added[0].PR != 148 || selection.PlanFrozen || selection.BenchmarkRuns != 0 {
		t.Fatal("selection provenance mismatch")
	}
}

func TestFrozenEvaluationRestoresFromRetainedSourceObjects(t *testing.T) {
	c := retainedFrozenCorpus(t)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	git, err = filepath.Abs(git)
	if err != nil {
		t.Fatal(err)
	}
	discover := exec.Command(git, "--no-replace-objects", "--no-lazy-fetch", "rev-parse", "--path-format=absolute", "--git-common-dir")
	discover.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1", "LC_ALL=C"}
	raw, err := discover.Output()
	if err != nil {
		t.Fatal("full local Git history required", err)
	}
	source := strings.TrimSpace(string(raw))
	root := t.TempDir()
	objects := filepath.Join(root, "source.git")
	command := exec.Command("python3", "testdata/evaluation/prepare-source.py", "--source-git-dir", source, "--output-dir", objects)
	command.Env = []string{"PATH=" + os.Getenv("PATH")}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatal("offline source provisioning", err, string(output))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store := GitStore{Git: git, GitDir: objects}
	for _, test := range c.Cases {
		packet, err := os.ReadFile(filepath.Join("testdata/evaluation/frozen", test.ID+".packet.json"))
		if err != nil {
			t.Fatal(err)
		}
		job := testJob()
		job.Snapshot = test.Snapshot
		job.Packet = string(packet)
		bound, err := RestoreSnapshotEvidence(ctx, store, job)
		if err != nil {
			t.Fatal(test.ID, err)
		}
		reader, err := bound.Reader(test.Snapshot)
		if err != nil {
			t.Fatal(err)
		}
		_, coverage := reader.Evidence()
		fresh, err := CaptureSnapshotEvidence(ctx, store, test.Snapshot)
		if err != nil || fresh.Packet() != string(packet) {
			t.Fatal("frozen packet cannot reproduce", test.ID, err)
		}
		want := fresh.Coverage()
		if len(coverage) != len(want) {
			t.Fatal("omission count changed", test.ID)
		}
		for i := range coverage {
			if coverage[i] != want[i] {
				t.Fatal("omission changed", test.ID)
			}
		}
	}
	// A damaged source bundle is refused before even creating the output store.
	bad := filepath.Join(root, "damaged")
	if err = os.Mkdir(bad, 0700); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile("testdata/evaluation/source-objects.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(bad, "source-objects.json"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
	bundle, err := os.ReadFile("testdata/evaluation/source-objects.bundle")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(bundle)
	var pinned struct{ BundleSHA256 string }
	if err = json.Unmarshal(manifest, &pinned); err != nil || hex.EncodeToString(sum[:]) != pinned.BundleSHA256 {
		t.Fatal("bundle pin mismatch", err)
	}
	bundle[len(bundle)-1] ^= 1
	if err = os.WriteFile(filepath.Join(bad, "source-objects.bundle"), bundle, 0600); err != nil {
		t.Fatal(err)
	}
	denied := filepath.Join(root, "denied.git")
	command = exec.Command("python3", "testdata/evaluation/prepare-source.py", "--source-git-dir", source, "--output-dir", denied, "--evaluation-dir", bad)
	command.Env = []string{"PATH=" + os.Getenv("PATH")}
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "bundle digest/size mismatch") {
		t.Fatal("corrupted bundle accepted", err, string(output))
	}
	if _, err = os.Stat(denied); !os.IsNotExist(err) {
		t.Fatal("damaged bundle created a store", err)
	}
}
