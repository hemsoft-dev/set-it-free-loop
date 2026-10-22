//go:build linux

package main

import (
	"encoding/json"
	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFakeEvaluationConfigRejectsUngatedMalformedAndOversizedInput(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "unopened-runtime")
	file := filepath.Join(dir, "config.json")
	c := config{Version: 1, Purpose: "six-arm-native-fake", Root: root, ImageSourceHead: strings.Repeat("a", 40)}
	c.Runtime.BootstrapLibraries = []r.VMBootAsset{}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if os.WriteFile(file, raw, 0600) != nil {
		t.Fatal("write config")
	}
	head, tree := strings.Repeat("b", 40), strings.Repeat("c", 40)
	args := []string{"evaluationfixture", "--run-fake-only", file}
	if got, err := readConfig(args, "1", head, tree); err != nil || got.Root != root {
		t.Fatal("valid structural config refused", err)
	}
	for _, arm := range []r.EvaluationArm{r.ArmAdversarial, r.ArmCodexControl, r.ArmClaudeControl, r.ArmCodex, "unknown"} {
		c.StoppedFaultArm = arm
		withFault, _ := json.Marshal(c)
		if os.WriteFile(file, withFault, 0600) != nil {
			t.Fatal("write fault config")
		}
		_, err := readConfig(args, "1", head, tree)
		want := arm == r.ArmAdversarial || arm == r.ArmCodexControl || arm == r.ArmClaudeControl
		if (err == nil) != want {
			t.Fatal("fault mode ingress mismatch", arm, err)
		}
	}
	c.StoppedFaultArm = ""
	if os.WriteFile(file, raw, 0600) != nil {
		t.Fatal("restore config")
	}
	cases := []struct {
		name                string
		args                []string
		enabled, head, tree string
	}{
		{"missing-opt-in", args, "", head, tree}, {"unknown-flag", []string{args[0], "--run", file}, "1", head, tree}, {"extra-flag", append(append([]string{}, args...), "--live"), "1", head, tree}, {"missing-args", nil, "1", head, tree}, {"unmeasured-head", args, "1", strings.Repeat("z", 40), tree}, {"unmeasured-tree", args, "1", head, strings.ToUpper(tree)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := readConfig(tc.args, tc.enabled, tc.head, tc.tree); err == nil {
				t.Fatal("ungated input accepted")
			}
		})
	}
	for _, value := range [][]byte{[]byte("null"), append(append([]byte{}, raw...), []byte(" {}")...), []byte(strings.Repeat(" ", 16385)), []byte(`{"Version":1,"unknown":true}`)} {
		if os.WriteFile(file, value, 0600) != nil {
			t.Fatal("write malformed config")
		}
		if _, err := readConfig(args, "1", head, tree); err == nil {
			t.Fatal("malformed config accepted")
		}
	}
	c.ImageSourceHead = strings.Repeat("x", 40)
	raw, _ = json.Marshal(c)
	if os.WriteFile(file, raw, 0600) != nil {
		t.Fatal("write invalid image")
	}
	if _, err := readConfig(args, "1", head, tree); err == nil {
		t.Fatal("invalid image provenance accepted")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("configuration parsing opened runtime state", err)
	}
}
