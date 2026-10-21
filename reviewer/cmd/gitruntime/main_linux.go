//go:build linux

// gitruntime builds a pinned, non-activating Git runtime artifact from explicit
// trusted installed inputs. It never executes Git, imports auth or starts units.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	reviewer "github.com/hemsoft-dev/set-it-free-loop/reviewer"
)

func writeExclusive(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	n, err := file.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return fmt.Errorf("short artifact write")
	}
	if err := file.Chmod(0444); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return file.Close()
}
func run() error {
	var git, http reviewer.VMBootAsset
	var output string
	flag.StringVar(&git.Path, "git", "", "absolute trusted Git executable")
	flag.StringVar(&git.SHA256, "git-sha256", "", "expected Git SHA-256")
	flag.Int64Var(&git.Bytes, "git-bytes", 0, "expected Git byte length")
	flag.StringVar(&http.Path, "http-helper", "", "absolute trusted git-remote-http executable")
	flag.StringVar(&http.SHA256, "http-sha256", "", "expected HTTP helper SHA-256")
	flag.Int64Var(&http.Bytes, "http-bytes", 0, "expected HTTP helper byte length")
	flag.StringVar(&output, "output-dir", "", "absolute new private output directory")
	flag.Parse()
	if flag.NArg() != 0 || !filepath.IsAbs(output) || filepath.Clean(output) != output {
		return fmt.Errorf("explicit canonical output directory required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	build, err := reviewer.BuildGitRuntime(ctx, git, http)
	if err != nil {
		return err
	}
	// Existing output is never adopted or overwritten. On a filesystem failure,
	// leave the private partial directory for inspection; it is not a release.
	if err := os.Mkdir(output, 0700); err != nil {
		return fmt.Errorf("new private output directory required")
	}
	artifact := filepath.Join(output, "git-runtime.bin")
	if err := writeExclusive(artifact, build.Artifact); err != nil {
		return fmt.Errorf("artifact persistence failed")
	}
	metadata := struct {
		Version                int                     `json:"version"`
		State                  string                  `json:"state"`
		Pins                   reviewer.GitRuntimePins `json:"pins"`
		Manifest               json.RawMessage         `json:"manifest"`
		Artifact               string                  `json:"artifact"`
		RuntimeQualified       bool                    `json:"runtime_qualified"`
		TrustedReleasePromoted bool                    `json:"trusted_release_promoted"`
	}{1, "built-only", build.Pins, build.Manifest, artifact, false, false}
	raw, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := writeExclusive(filepath.Join(output, "manifest.json"), raw); err != nil {
		return fmt.Errorf("manifest persistence failed")
	}
	directory, err := os.Open(output)
	if err != nil {
		return err
	}
	err = directory.Sync()
	directory.Close()
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		State, Manifest, Artifact string
		Pins                      reviewer.GitRuntimePins
	}{"built-only", filepath.Join(output, "manifest.json"), artifact, build.Pins})
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Git runtime build refused:", err)
		os.Exit(1)
	}
}
