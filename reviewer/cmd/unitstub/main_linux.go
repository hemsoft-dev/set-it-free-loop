//go:build linux

// unitstub is a fixed fake-only payload. Before grant it starts no child. After
// grant it hashes the marker and starts only its own fixed inert escaped-child
// mode, allowing the explicit host fixture to verify whole-cgroup termination.
package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	"golang.org/x/sys/unix"
)

func main() {
	if len(os.Args) == 3 && os.Args[1] == "--fake-escaped-child" {
		// Readiness is written only after TERM is ignored and setsid is verified.
		// The parent supplies this one fixed receipt name from its trusted gate.
		if !filepath.IsAbs(os.Args[2]) || filepath.Clean(os.Args[2]) != os.Args[2] || filepath.Base(os.Args[2]) != "fake-child.json" {
			os.Exit(2)
		}
		signal.Ignore(syscall.SIGTERM)
		session, err := unix.Getsid(0)
		if err != nil || session != os.Getpid() {
			os.Exit(1)
		}
		raw, _ := json.Marshal(struct{ Version, PID, Session int }{1, os.Getpid(), session})
		if os.WriteFile(os.Args[2], raw, 0600) != nil {
			os.Exit(1)
		}
		time.Sleep(90 * time.Second)
		return
	}
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
	self, err := os.Executable()
	if err != nil {
		os.Exit(1)
	}
	// No model-selected command or path. Only this pinned executable's fixed
	// fake-child mode is launched, after the authenticated manifest grant.
	child := exec.Command(self, "--fake-escaped-child", filepath.Join(filepath.Dir(os.Args[1]), "fake-child.json"))
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if child.Start() != nil {
		os.Exit(1)
	}
	child.Process.Release()
}
