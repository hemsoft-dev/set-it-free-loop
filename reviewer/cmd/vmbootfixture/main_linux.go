//go:build linux

// vmbootfixture is opt-in fake-only VM boot qualification, not an activation
// command or native worker launcher. It refuses ordinary host invocation.
package main

import (
	"context"
	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	"os"
)

func main() { os.Exit(r.VMBootFixtureMain(context.Background(), os.Args[1:], os.Stdout)) }
