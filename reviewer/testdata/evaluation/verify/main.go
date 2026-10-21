// This fixture verifier reads an already provisioned trusted object store. It
// never fetches, runs candidate code, starts providers or promotes a release.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"
)

func read(path string, max int) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	raw, e := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if e != nil {
		return nil, e
	}
	if len(raw) > max {
		return nil, errors.New("fixture exceeds byte limit")
	}
	return raw, nil
}
func run(args []string) error {
	if len(args) != 2 || !filepath.IsAbs(args[0]) {
		return errors.New("usage: go run ./testdata/evaluation/verify ABS_GIT_DIR EVALUATION_DIR")
	}
	directory := args[1]
	raw, e := read(filepath.Join(directory, "frozen", "corpus.json"), 16<<20)
	if e != nil {
		return e
	}
	corpus, e := r.DecodeStrict[r.EvaluationCorpus](raw, 16<<20)
	if e != nil {
		return e
	}
	if e = corpus.Validate(); e != nil {
		return e
	}
	hash, e := read(filepath.Join(directory, "frozen", "corpus.sha256"), 65)
	if e != nil {
		return e
	}
	if string(hash) != corpus.Hash()+"\n" {
		return errors.New("corpus digest mismatch")
	}
	git, e := exec.LookPath("git")
	if e != nil {
		return e
	}
	git, e = filepath.Abs(git)
	if e != nil {
		return e
	}
	store := r.GitStore{Git: git, GitDir: args[0]}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	type checkedCase struct {
		ID, PacketHash  string
		Bytes, Coverage int
	}
	rows := []checkedCase{}
	clean, defects := 0, 0
	for _, test := range corpus.Cases {
		if !regexp.MustCompile(`^[a-z0-9-]{1,64}$`).MatchString(test.ID) {
			return errors.New("unsafe fixture case ID")
		}
		packet, e := read(filepath.Join(directory, "frozen", test.ID+".packet.json"), r.EvaluationPacketBytes)
		if e != nil {
			return e
		}
		if len(packet) != test.SerializedPacketBytes || r.Digest(string(packet)) != test.PacketHash {
			return errors.New("packet digest/size mismatch")
		}
		config := r.DefaultConfig()
		config.Limits.OutputBytes = r.EvaluationOutputBytes
		// A representative job binds restoration only. It is never admitted/run,
		// and this source reference supplies no trusted-release activation grant.
		job := r.Job{ID: test.ID, RequesterID: r.OwnerID, Generation: 1, Snapshot: test.Snapshot, Config: config, ConfigVersion: 1, Release: "11f11c746966303d34c86f575b99da796249ae1e", Packet: string(packet)}
		restored, e := r.RestoreSnapshotEvidence(ctx, store, job)
		if e != nil {
			return fmt.Errorf("%s: %w", test.ID, e)
		}
		reader, e := restored.Reader(test.Snapshot)
		if e != nil {
			return e
		}
		_, coverage := reader.Evidence()
		var original struct{ Coverage []r.Coverage }
		if e = json.Unmarshal(packet, &original); e != nil {
			return e
		}
		if len(coverage) != len(original.Coverage) {
			return errors.New("restored omission count differs")
		}
		for i := range coverage {
			if coverage[i] != original.Coverage[i] {
				return errors.New("restored omission differs")
			}
		}
		// Reproduce the pinned producer's exact packet, not just a schema-valid copy.
		fresh, e := r.CaptureSnapshotEvidence(ctx, store, test.Snapshot)
		if e != nil {
			return e
		}
		if fresh.Packet() != string(packet) {
			return errors.New("recapture changed frozen evidence")
		}
		rows = append(rows, checkedCase{test.ID, test.PacketHash, len(packet), len(coverage)})
		if test.Label.Clean {
			clean++
		} else {
			defects++
		}
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Scope, CorpusHash                   string
		Cases, Clean, Defect, BenchmarkRuns int
		Checked                             []checkedCase
	}{"offline corpus integrity and source restoration only", corpus.Hash(), len(rows), clean, defects, 0, rows})
}
func main() {
	if e := run(os.Args[1:]); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
