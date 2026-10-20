//go:build linux

package reviewer

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRetainedStubSnapshotChild(t *testing.T) {
	if os.Getenv("SFL_RETAINED_STUB_CHILD") != "1" {
		return
	}
	fmt.Println("verified inode")
	io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}
func TestRetainedStubExecutesVerifiedInodeAfterAtomicReplacement(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	original := filepath.Join(root, "original")
	if err = os.WriteFile(original, raw, 0500); err != nil {
		t.Fatal(err)
	}
	image, err := retainStubImage(root, original, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer image.Close()
	replacement := filepath.Join(root, "replacement")
	if err = os.WriteFile(replacement, []byte("unpinned replacement"), 0500); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(replacement, original); err != nil {
		t.Fatal(err)
	}
	if _, err = unix.Write(int(image.file.Fd()), []byte("overwrite")); !errors.Is(err, unix.EPERM) {
		t.Fatal("image was writable", err)
	}
	if err = unix.Ftruncate(int(image.file.Fd()), 1); !errors.Is(err, unix.EPERM) {
		t.Fatal("image could shrink", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, retainedStubExec, "-test.run=^TestRetainedStubSnapshotChild$")
	cmd.ExtraFiles = []*os.File{image.file}
	cmd.Env = []string{"SFL_RETAINED_STUB_CHILD=1"}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "verified inode\n" {
		t.Fatal("executed replacement pathname", line, err)
	}
	if err = verifyRunningStub(cmd.Process.Pid, hash); err != nil {
		t.Fatal("running image lacks seals or pin", err)
	}
	stdin.Close()
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	// A new controller can recover the original pin from its private cache even
	// when the caller's original pathname now contains different bytes.
	reopened, err := retainStubImage(root, original, hash)
	if err != nil {
		t.Fatal("lost restart image", err)
	}
	reopened.Close()
	if err = verifyRunningStub(os.Getpid(), hash); err == nil {
		t.Fatal("unsealed ambient process accepted")
	}
}
