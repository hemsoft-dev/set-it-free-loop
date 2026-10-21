//go:build linux

// gitacquire is a fixed, credential-free acquisition payload. A trusted factory
// must qualify its unit, physical boundaries and grant recipients before use.
// Building it is offline tooling, not deployment or activation.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer cancel()
	if r.GitAcquisitionProcess(ctx, os.Args[1:], os.Getenv("INVOCATION_ID")) != nil {
		os.Exit(1)
	}
}
