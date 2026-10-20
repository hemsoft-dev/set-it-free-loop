//go:build linux

// containmentstub is deliberately fake-only. A granted marker produces its
// digest; it cannot start clients, access profiles or execute a command.
package main

import (
	"context"
	"os"
	"strconv"
	"time"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
)

func main() {
	if len(os.Args) != 7 || os.Args[5] != "--fake-marker-only" {
		os.Exit(2)
	}
	binding, err := r.DecodeStrict[r.AuthBinding]([]byte(os.Args[2]), 1024)
	if err != nil {
		os.Exit(2)
	}
	supervisor, err := strconv.Atoi(os.Args[6])
	if err != nil {
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if r.ContainmentStubFake(ctx, os.Args[1], binding, os.Args[3], os.Args[4], supervisor, os.Stdout) != nil {
		os.Exit(1)
	}
}
