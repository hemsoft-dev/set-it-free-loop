// This offline fixture tool materializes the frozen corpus using the actual
// trusted evidence producer. It does not fetch, run candidates or run models.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	reviewer "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
)

func run(args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: go run ./testdata/evaluation/freeze ABS_GIT_DIR LABELS.json NEW_OUTPUT_DIR")
	}
	if !filepath.IsAbs(args[0]) {
		return fmt.Errorf("use an absolute trusted Git object directory")
	}
	raw, err := os.ReadFile(args[1])
	if err != nil {
		return err
	}
	c, err := reviewer.DecodeStrict[reviewer.EvaluationCorpus](raw, 16<<20)
	if err != nil {
		return err
	}
	if len(c.Cases) < 20 || len(c.Cases) > 100 {
		return fmt.Errorf("invalid draft case count")
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return err
	}
	gitPath, err = filepath.Abs(gitPath)
	if err != nil {
		return err
	}
	packets := map[string]string{}
	for i := range c.Cases {
		test := &c.Cases[i]
		if !regexp.MustCompile(`^[a-z0-9-]{1,64}$`).MatchString(test.ID) {
			return fmt.Errorf("unsafe case ID")
		}
		if err = test.Snapshot.Validate(); err != nil {
			return err
		}
		if err = test.ValidateLabels(c.FrozenAt); err != nil {
			return err
		}
		evidence, e := reviewer.CaptureSnapshotEvidence(context.Background(), reviewer.GitStore{Git: gitPath, GitDir: args[0]}, test.Snapshot)
		if e != nil {
			return fmt.Errorf("%s: %w", test.ID, e)
		}
		packet := evidence.Packet()
		test.PacketHash = reviewer.Digest(packet)
		test.SerializedPacketBytes = len(packet)
		packets[test.ID] = packet
	}
	if err = c.Validate(); err != nil {
		return err
	}
	// A new directory prevents accidental replacement of already frozen evidence.
	if err = os.Mkdir(args[2], 0755); err != nil {
		return err
	}
	for id, packet := range packets {
		if err = os.WriteFile(filepath.Join(args[2], id+".packet.json"), []byte(packet), 0644); err != nil {
			return err
		}
	}
	raw, err = json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(args[2], "corpus.json"), append(raw, '\n'), 0644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(args[2], "corpus.sha256"), []byte(c.Hash()+"\n"), 0644)
}
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
