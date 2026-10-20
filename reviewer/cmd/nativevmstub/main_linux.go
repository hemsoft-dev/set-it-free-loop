//go:build linux

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
	if r.NativeVMStub(context.Background(), os.Args[1], binding, os.Getenv("INVOCATION_ID"), os.Args[3], supervisor) != nil {
		os.Exit(1)
	}
}
