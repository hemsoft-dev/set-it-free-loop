//go:build linux

// unitstub is the fixed inert systemd payload. Only INVOCATION_ID is read from
// its launch environment. It hashes a synthetic granted marker and exits.
package main

import (
	"context"
	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	"os"
	"strconv"
	"time"
)

func main() {
	if len(os.Args) != 5 {
		os.Exit(2)
	}
	binding, err := r.DecodeStrict[r.AuthBinding]([]byte(os.Args[2]), 1024)
	if err != nil {
		os.Exit(2)
	}
	supervisor, err := strconv.Atoi(os.Args[4])
	if err != nil {
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if r.ContainmentStubFake(ctx, os.Args[1], binding, os.Getenv("INVOCATION_ID"), os.Args[3], supervisor, os.Stdout) != nil {
		os.Exit(1)
	}
}
