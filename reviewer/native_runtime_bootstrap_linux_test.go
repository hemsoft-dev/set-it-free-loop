//go:build linux

package reviewer

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNativeRuntimeOptInCapturedNamespaceLaunch(t *testing.T) {
	artifact := os.Getenv("SFL_NATIVE_RUNTIME_ARTIFACT")
	helperPath := os.Getenv("SFL_NATIVE_RUNTIME_HELPER")
	if artifact == "" || helperPath == "" {
		t.Skip("explicit captured runtime and original namespace helper not selected")
	}
	bytes, err := strconv.ParseInt(os.Getenv("SFL_NATIVE_RUNTIME_QEMU_BYTES"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	pin := VMAssetPin{os.Getenv("SFL_NATIVE_RUNTIME_QEMU_SHA256"), bytes}
	input, err := os.Open(artifact)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	qemu, err := sealVMAsset(ctx, VMBootAsset{os.Getenv("SFL_NATIVE_RUNTIME_QEMU_PATH"), pin.SHA256, pin.Bytes}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer qemu.Close()
	prepared, err := prepareNativeRuntime(nativeRuntimePrivateParent(t), input, pin, io.NewSectionReader(qemu, 0, pin.Bytes+1))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := prepared.Close(); err != nil {
			t.Error(err)
		}
	}()
	root, err := prepared.directoryFD()
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	helperBytes, err := strconv.ParseInt(os.Getenv("SFL_NATIVE_RUNTIME_HELPER_BYTES"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	helper, err := retainNativeBootstrap(ctx, VMBootAsset{helperPath, os.Getenv("SFL_NATIVE_RUNTIME_HELPER_SHA256"), helperBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer helper.Close()
	inert, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer inert.Close()
	files := make([]*os.File, nativeRuntimeRootFD-2)
	for index := range files {
		files[index] = inert
	}
	files[0] = qemu
	files[nativeRuntimeHelperFD-3] = helper
	files[nativeRuntimeRootFD-3] = root
	arguments := []string{"--no-new-privs", "--", "/proc/self/fd/" + strconv.Itoa(nativeRuntimeHelperFD)}
	arguments = append(arguments, nativeRuntimeNamespaceArgs([]string{"--version"})...)
	command := exec.CommandContext(ctx, "/usr/bin/setpriv", arguments...)
	command.Env = []string{"LANG=C", "LC_ALL=C"}
	command.Dir = "/"
	command.ExtraFiles = files
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "QEMU emulator version 10.2.1") {
		t.Fatalf("captured namespace launch: %v %s", err, output)
	}
	// No KVM device or provider descriptors are supplied in this metadata-only run.
}
