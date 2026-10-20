package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	reviewer "github.com/hemsoft-dev/set-it-free-loop/reviewer"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := reviewer.SourceProxyMain(ctx, os.Args[1:], os.Stdin, os.Stdout)
	stop()
	if code != 0 {
		fmt.Fprintln(os.Stderr, "source proxy failed")
	}
	os.Exit(code)
}
