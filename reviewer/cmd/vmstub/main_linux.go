//go:build linux

// vmstub is the fixed credential-free VM payload. Only an authenticated
// manager-bound descriptor grant can provide its boot images and KVM device.
package main

import (
	"context"
	"os"
	"strconv"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
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
	if r.VMFixtureStub(context.Background(), os.Args[1], binding, os.Getenv("INVOCATION_ID"), os.Args[3], supervisor) != nil {
		os.Exit(1)
	}
}
