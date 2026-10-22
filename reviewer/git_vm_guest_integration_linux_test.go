//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Explicit model-free fixture only. The outer diagnostic unit must bound the
// test and QEMU. This proves the guest data path, not a production host launch,
// durable recovery, provider eligibility, auth release or trusted promotion.
func TestGitVMGuestContainedActualGit(t *testing.T) {
	directory := os.Getenv("SFL_SOURCE_GUEST_IMAGE")
	if directory == "" {
		t.Skip("explicit source-only image and bounded diagnostic unit required")
	}
	raw, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var image struct {
		Purpose              string            `json:"purpose"`
		Release              gitVMGuestRelease `json:"release"`
		Kernel, Initrd, BIOS VMAssetPin
		Host                 struct {
			Manifest struct{ Program VMAssetPin }
			Artifact VMAssetPin
		} `json:"host_runtime"`
	}
	if json.Unmarshal(raw, &image) != nil || image.Purpose != "source-vm-image" || !image.Release.valid() {
		t.Fatal("invalid actual image metadata")
	}
	for name, pin := range map[string]VMAssetPin{"vmlinuz": image.Kernel, "source.cpio.gz": image.Initrd, "bios.bin": image.BIOS} {
		data, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil || int64(len(data)) != pin.Bytes || gitRawHash(data) != pin.SHA256 {
			t.Fatalf("image asset changed: %s", name)
		}
	}
	store, snapshot := gitQuarantineFixture(t)
	q := gitVMGuestTestRequest(t)
	m := &q.Source.Manifest
	m.Kernel = image.Kernel
	m.Initrd = image.Initrd
	m.BIOS = image.BIOS
	m.QEMU = image.Host.Manifest.Program
	m.QEMURuntime = image.Host.Artifact
	m.Worker = image.Release.Worker
	m.NamespaceHelper = image.Release.NamespaceHelper
	m.Runtime = image.Release.Runtime
	m.BootstrapLibraries = image.Release.BootstrapLibraries
	q.Source.Binding.Snapshot = snapshot
	q.Source.Binding.RuntimeHash = Digest(*m)
	q.Read.Source = q.Source.Binding
	if !q.valid() || !image.Release.matches(q) {
		t.Fatal("image/request mismatch")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	var channels []net.Conn
	var peers []*os.File
	for i := 0; i < 3; i++ {
		pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		host := os.NewFile(uintptr(pair[0]), "fixture-host")
		conn, err := net.FileConn(host)
		host.Close()
		if err != nil {
			unix.Close(pair[1])
			t.Fatal(err)
		}
		channels = append(channels, conn)
		peers = append(peers, os.NewFile(uintptr(pair[1]), "fixture-guest"))
	}
	defer func() {
		for _, c := range channels {
			c.Close()
		}
		closeVMFiles(peers)
	}()
	args := []string{"-no-user-config", "-nodefaults", "-machine", "q35,dump-guest-core=off,mem-merge=off", "-accel", "kvm", "-cpu", "host", "-m", "1536M", "-smp", "1", "-display", "none", "-monitor", "none", "-nic", "none", "-no-reboot", "-bios", filepath.Join(directory, "bios.bin"), "-kernel", filepath.Join(directory, "vmlinuz"), "-initrd", filepath.Join(directory, "source.cpio.gz"), "-append", "console=ttyS0 loglevel=3 initramfs_options=size=1024m,nr_inodes=4096 rdinit=/source-init panic=1 sfl-source=" + Digest(q), "-serial", "stdio", "-device", "virtio-serial-pci,id=sflserial"}
	for i, name := range []string{"source-control", "source-read", "source-export"} {
		id := "sfl" + strconv.Itoa(i)
		args = append(args, "-chardev", fmt.Sprintf("socket,id=%s,fd=%d", id, i+3), "-device", "virtserialport,bus=sflserial.0,chardev="+id+",name=sfl."+name)
	}
	args = append(args, "-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny")
	cmd := exec.CommandContext(ctx, "/usr/bin/qemu-system-x86_64", args...)
	cmd.ExtraFiles = peers
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	closeVMFiles(peers)
	peers = nil
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("fixture QEMU did not exit")
		}
	}()
	stop := gitBrokerAfterFunc(ctx, func() {
		for _, c := range channels {
			c.Close()
		}
	})
	defer stop()
	readDone := make(chan error, 1)
	go func() {
		_, err := serveGitVMRead(ctx, channels[1], q.Read, gitExchangeFunc(func(ctx context.Context, r GitHubGitReadRequest) (io.ReadCloser, error) {
			args := []string{"upload-pack", "--stateless-rpc"}
			if r.Operation == GitHubGitAdvertisement {
				args = append(args, "--advertise-refs")
			}
			args = append(args, store.GitDir)
			git := exec.CommandContext(ctx, store.Git, args...)
			git.Env = []string{"PATH=/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "LC_ALL=C"}
			if r.Protocol == 2 {
				git.Env = append(git.Env, "GIT_PROTOCOL=version=2")
			}
			if r.Body != nil {
				defer r.Body.Close()
				git.Stdin = r.Body
			}
			data, err := git.Output()
			if err != nil {
				return nil, err
			}
			if r.Operation == GitHubGitAdvertisement && r.Protocol != 2 {
				data = append([]byte("001e# service=git-upload-pack\n0000"), data...)
			}
			return io.NopCloser(bytes.NewReader(data)), nil
		}), func(context.Context, gitVMReadBinding) error { return nil })
		readDone <- err
	}()
	defer func() { cancel(); channels[1].Close(); <-readDone }()
	if err := writeVMRecord(channels[0], q, gitVMRecordBytes); err != nil {
		t.Fatal(err)
	}
	ready, err := readVMRecord[gitVMGuestPhase](channels[0], gitVMRecordBytes)
	if err != nil || !ready.valid(q, "guest-ready") {
		t.Fatalf("guest readiness: %v %+v", err, ready)
	}
	if err := writeVMRecord(channels[0], gitVMGuestPhase{1, gitVMGuestPurpose, Digest(q), "host-grant"}, gitVMRecordBytes); err != nil {
		t.Fatal(err)
	}
	materialized, err := receiveGitVMExport(ctx, channels[2], q.Source)
	if err != nil {
		t.Fatal("guest export", err)
	}
	defer materialized.Close()
	finished, err := readVMRecord[gitVMGuestFinished](channels[0], gitVMRecordBytes)
	if err != nil || !finished.valid(q) {
		t.Fatalf("guest completion: %v %+v", err, finished)
	}
	object, err := materialized.Read(ctx, snapshot.Head, "safe.go", snapshotObjectBytes)
	want, wantErr := store.Read(ctx, snapshot.Head, "safe.go", snapshotObjectBytes)
	if err != nil || wantErr != nil || !bytes.Equal(object.Data, want.Data) {
		t.Fatalf("guest source differs: %v %v", err, wantErr)
	}
	t.Logf("actual source-only guest exported %d bytes through %d broker requests; host supervisor qualification remains separate", finished.ExportBytes, finished.Broker.Requests)
}
