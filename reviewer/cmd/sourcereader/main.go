package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/hemsoft-dev/set-it-free-loop/reviewer"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := 2
	if len(os.Args) == 1 {
		code = reviewer.SourceReaderMain(ctx, os.Environ(), os.Stdin, os.Stdout)
	}
	cancel()
	if code != 0 {
		fmt.Fprintln(os.Stderr, "private source reader failed")
	}
	os.Exit(code)
}
