//go:build linux

// vmgrantfixture opts into one credential-free systemd/KVM grant exercise.
package main

import (
	"context"
	"os"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
)

func main() { os.Exit(r.VMGrantFixtureMain(context.Background(), os.Args[1:], os.Stdout)) }
