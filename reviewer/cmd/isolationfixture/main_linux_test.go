//go:build linux

package main

import (
	"os"
	"os/exec"
	"testing"
)

func TestGuestPrivilegeSetupRefusesAnOrdinaryHostProcess(t *testing.T) {
	if os.Getenv("SFL_GUEST_GUARD_CHILD") == "1" {
		guestInit()
		os.Exit(90)
	}
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(program, "-test.run=^TestGuestPrivilegeSetupRefusesAnOrdinaryHostProcess$")
	child.Env = []string{"SFL_GUEST_GUARD_CHILD=1"}
	output, err := child.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 2 || len(output) != 0 {
		t.Fatal("ordinary host process reached guest privilege setup", string(output), err)
	}
}
