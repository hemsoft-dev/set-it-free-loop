//go:build linux

// This fixed entry point refuses outside the source guest and has no host
// activation command, credential loader or provider execution path.
package main

import (
	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	"os"
)

func main() { os.Exit(r.GitVMGuestMain(os.Args[1:])) }
