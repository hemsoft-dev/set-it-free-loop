//go:build linux

package main

import (
	"os"
	"path/filepath"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
)

func main() {
	args := os.Args[1:]
	if filepath.Base(os.Args[0]) == "codex-code-mode-host" {
		if len(args) != 0 {
			os.Exit(2)
		}
		args = []string{"--native-codehost-relay"}
	}
	os.Exit(r.NativeVMGuestMain(args))
}
