//go:build linux

// gitsourcevmstub is the fixed, credential-free source VM unit payload. A
// trusted source factory must start, capture, grant and stop its original unit.
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
	if r.GitVMSourceStub(ctx, os.Args[1:], os.Getenv("INVOCATION_ID")) != nil {
		os.Exit(1)
	}
}
