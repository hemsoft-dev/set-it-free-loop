//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	if os.Getenv("SFL_VM_ASSET_HELPER") == "1" {
		contents := []string{}
		for _, fd := range []uintptr{4, 5} {
			file := os.NewFile(fd, "asset")
			raw, err := io.ReadAll(io.LimitReader(file, 1024))
			if err != nil {
				os.Exit(2)
			}
			contents = append(contents, string(raw))
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"contents": contents, "args": os.Args[1:], "canary": os.Getenv("SFL_VM_PARENT_CANARY")})
		_, _ = os.Stdin.Read(make([]byte, 1))
		os.Exit(0)
	}
	os.Exit(m.Run())
}
func vmAsset(t *testing.T, path string, raw []byte) VMBootAsset {
	t.Helper()
	if err := os.WriteFile(path, raw, 0500); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	return VMBootAsset{Path: path, SHA256: hex.EncodeToString(hash[:]), Bytes: int64(len(raw))}
}
func vmSpec(t *testing.T) VMBootSpec {
	root := t.TempDir()
	return VMBootSpec{Version: 1, Binding: AuthBinding{SessionHash: Digest("fake-session"), LaunchID: strings.Repeat("1", 32), ContainmentID: Digest("fake-containment")}, QEMU: vmAsset(t, filepath.Join(root, "qemu"), []byte("fake-executable")), Kernel: vmAsset(t, filepath.Join(root, "kernel"), []byte("original-kernel")), Initrd: vmAsset(t, filepath.Join(root, "initrd"), []byte("original-initrd"))}
}
func TestVMBootSealsSnapshotsAndRejectsMutation(t *testing.T) {
	spec := vmSpec(t)
	images, err := OpenVMBootImages(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer images.Close()
	for index, asset := range []VMBootAsset{spec.QEMU, spec.Kernel, spec.Initrd} {
		if err := os.Rename(asset.Path, asset.Path+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(asset.Path, []byte("replacement"), 0500); err != nil {
			t.Fatal(err)
		}
		file := images.files[index]
		if _, err := file.WriteAt([]byte("bad"), 0); err == nil {
			t.Fatal("sealed boot asset writable")
		}
		if err := file.Truncate(0); err == nil {
			t.Fatal("sealed boot asset shrinkable")
		}
		mode := os.FileMode(0500)
		if index == 0 {
			mode = 0400
		}
		if err := file.Chmod(mode); err == nil {
			t.Fatal("sealed execute bits can change")
		}
		raw, err := io.ReadAll(io.NewSectionReader(file, 0, asset.Bytes))
		sum := sha256.Sum256(raw)
		if err != nil || hex.EncodeToString(sum[:]) != asset.SHA256 {
			t.Fatal("retained bytes changed", index, err)
		}
		if index > 0 {
			info, _ := file.Stat()
			if info.Mode().Perm()&0111 != 0 {
				t.Fatal("data boot asset executable")
			}
		}
	}
	copies, err := images.duplicate()
	if err != nil {
		t.Fatal(err)
	}
	defer closeVMFiles(copies)
	if err := images.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(io.NewSectionReader(copies[1], 0, spec.Kernel.Bytes))
	if err != nil || string(raw) != "original-kernel" {
		t.Fatal("closing owner invalidated child snapshot", err)
	}
	if _, err := images.duplicate(); !errors.Is(err, ErrVMBoot) {
		t.Fatal("closed images reused", err)
	}
}
func TestVMBootRejectsUnsafePinsAndFiles(t *testing.T) {
	for _, kind := range []string{"hash", "size", "oversize", "symlink", "hardlink", "writable", "binding", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			spec := vmSpec(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "hash":
				spec.Kernel.SHA256 = strings.Repeat("0", 64)
			case "size":
				spec.Initrd.Bytes++
			case "oversize":
				spec.Initrd.Bytes = (512 << 20) + 1
			case "symlink":
				os.Rename(spec.Kernel.Path, spec.Kernel.Path+".old")
				os.Symlink(spec.Kernel.Path+".old", spec.Kernel.Path)
			case "hardlink":
				os.Link(spec.Initrd.Path, spec.Initrd.Path+".link")
			case "writable":
				os.Chmod(spec.Kernel.Path, 0666)
			case "binding":
				spec.Binding.LaunchID = "../../invalid"
			case "cancelled":
				cancel()
			}
			if images, err := OpenVMBootImages(ctx, spec); !errors.Is(err, ErrVMBoot) || images != nil {
				if images != nil {
					images.Close()
				}
				t.Fatal("unsafe asset accepted", err)
			}
		})
	}
}
func TestVMBootActualExecutableUsesRetainedDescriptors(t *testing.T) {
	spec := vmSpec(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(spec.QEMU.Path); err != nil {
		t.Fatal(err)
	}
	spec.QEMU = vmAsset(t, spec.QEMU.Path, raw)
	images, err := OpenVMBootImages(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer images.Close()
	if err := os.Rename(spec.QEMU.Path, spec.QEMU.Path+".retired"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spec.QEMU.Path, []byte("not-an-executable"), 0500); err != nil {
		t.Fatal(err)
	}
	fakeKVM, err := os.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	defer fakeKVM.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd, files, err := images.command(ctx, fakeKVM)
	if err != nil {
		t.Fatal(err)
	}
	defer closeVMFiles(files)
	t.Setenv("SFL_VM_PARENT_CANARY", "must-not-inherit")
	cmd.Env = append(cmd.Env, "SFL_VM_ASSET_HELPER=1")
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer writer.Close()
	cmd.Stdin = input
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	imageErr := verifyRunningStub(cmd.Process.Pid, spec.QEMU.SHA256)
	_, _ = writer.Write([]byte{1})
	waitErr := cmd.Wait()
	if imageErr != nil || waitErr != nil {
		t.Fatal("descriptor executable not verified", imageErr, waitErr, output.String())
	}
	var observed struct {
		Contents []string
		Args     []string
		Canary   string
	}
	if err := json.Unmarshal(output.Bytes(), &observed); err != nil {
		t.Fatal(err, output.String())
	}
	if !reflect.DeepEqual(observed.Contents, []string{"original-kernel", "original-initrd"}) || !reflect.DeepEqual(observed.Args, vmFixtureArgs(spec.Binding.LaunchID)) || observed.Canary != "" {
		t.Fatal("child descriptors/arguments/environment changed", observed)
	}
	for _, file := range files[:3] {
		seals, err := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
		if err != nil || seals&requiredStubSeals != requiredStubSeals {
			t.Fatal("child image lost seals", err)
		}
	}
}
func TestVMBootFixtureRequiresContainingUnit(t *testing.T) {
	spec := vmSpec(t)
	images, err := OpenVMBootImages(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer images.Close()
	t.Setenv("INVOCATION_ID", "")
	if verified, err := images.RunVMBootFixture(context.Background(), io.Discard); verified || !errors.Is(err, ErrVMBoot) {
		t.Fatal("fixture ran outside owned unit", verified, err)
	}
	var target bytes.Buffer
	limited := vmSerialWriter{target: &target, remaining: 2}
	if _, err := limited.Write([]byte("abc")); err == nil || !limited.failed || target.Len() != 0 {
		t.Fatal("serial bound bypassed")
	}
}

func TestVMBootFixtureRequiresLinkedRevisionAndTree(t *testing.T) {
	originalHead, originalTree := vmFixtureBuildRevision, vmFixtureBuildTree
	defer func() { vmFixtureBuildRevision, vmFixtureBuildTree = originalHead, originalTree }()
	config := VMBootFixtureConfig{Version: 1, Purpose: "synthetic-role-fixture", SourceHead: strings.Repeat("a", 40), SourceTree: strings.Repeat("b", 40), Boot: vmSpec(t)}
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "fixture.json")
	write := func() {
		raw, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	vmFixtureBuildRevision, vmFixtureBuildTree = "", ""
	if _, err := readVMBootFixture(path); !errors.Is(err, ErrVMBoot) {
		t.Fatal("generic build accepted release identity", err)
	}
	vmFixtureBuildRevision, vmFixtureBuildTree = config.SourceHead, config.SourceTree
	if _, err := readVMBootFixture(path); err != nil {
		t.Fatal("correct linked build rejected", err)
	}
	config.SourceTree = strings.Repeat("c", 40)
	write()
	if _, err := readVMBootFixture(path); !errors.Is(err, ErrVMBoot) {
		t.Fatal("different source tree accepted", err)
	}
	config.SourceTree = vmFixtureBuildTree
	config.SourceHead = strings.Repeat("c", 40)
	write()
	if _, err := readVMBootFixture(path); !errors.Is(err, ErrVMBoot) {
		t.Fatal("different revision accepted", err)
	}
}
