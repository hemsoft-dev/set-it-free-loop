//go:build linux

package reviewer

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestNativeGuestDiagnosticRetainsOnlyBoundedEnums(t *testing.T) {
	d := &nativeGuestClientDiagnostics{}
	d.Write([]byte("certifi"))
	d.Write([]byte("cate failed, sensitive synthetic bytes"))
	if d.exitCode(nil) != 201 || bytes.Contains(d.tail, []byte("sensitive")) {
		t.Fatal("TLS class or suffix clearing failed")
	}
	large := bytes.Repeat([]byte("opaque diagnostic "), 100000)
	if n, err := d.Write(large); err != nil || n != len(large) || d.scanned != 64<<10 || len(d.tail) > 128 {
		t.Fatal("unbounded stderr scan")
	}
	d.exitCode(nil)
	if !bytes.Equal(d.tail, make([]byte, len(d.tail))) {
		t.Fatal("raw suffix retained")
	}
}

func TestNativeGuestClientStatusIsOptionalBoundAndFinite(t *testing.T) {
	q := nativeRequestFixture(t, Codex)
	good := nativeGuestClientStatus{1, q.Binding, Digest(q), "provider-url"}
	for _, kind := range []string{"valid", "empty", "binding", "request", "class", "version", "extra", "truncated"} {
		t.Run(kind, func(t *testing.T) {
			status := good
			switch kind {
			case "binding":
				status.Binding.LaunchID = "different"
			case "request":
				status.RequestHash = Digest("other")
			case "class":
				status.Class = "untrusted diagnostic bytes"
			case "version":
				status.Version = 2
			}
			var frame bytes.Buffer
			if kind != "empty" {
				if writeVMRecord(&frame, status, 4096) != nil {
					t.Fatal("status fixture")
				}
			}
			if kind == "extra" {
				frame.WriteByte(1)
			}
			raw := frame.Bytes()
			if kind == "truncated" {
				raw = raw[:len(raw)-1]
			}
			read, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer read.Close()
			done := make(chan struct{})
			go func() { write.Write(raw); write.Close(); close(done) }()
			got := readNativeGuestClientStatus(read, q)
			<-done
			want := "diagnostic-invalid"
			if kind == "valid" {
				want = good.Class
			}
			if kind == "empty" {
				want = ""
			}
			if got != want {
				t.Fatal("private status accepted wrong authority/framing", kind, got)
			}
		})
	}
}

func TestNativeGuestReaderSignalChild(t *testing.T) {
	if os.Getenv("SFL_TEST_READER_SIGNAL") != "1" {
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer cancel()
	fmt.Fprintln(os.Stdout, "ready")
	<-ctx.Done()
}

func TestNativeGuestStopCancelsAnIdleReaderBeforeCleanupDeadline(t *testing.T) {
	group := filepath.Join(t.TempDir(), "native-reader")
	if os.Mkdir(group, 0700) != nil || os.WriteFile(filepath.Join(group, "cgroup.kill"), []byte("0"), 0600) != nil || os.WriteFile(filepath.Join(group, "cgroup.events"), []byte("populated 0\n"), 0600) != nil {
		t.Fatal("fake cgroup setup")
	}
	fd, err := os.Open(group)
	if err != nil {
		t.Fatal(err)
	}
	input, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	read, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNativeGuestReaderSignalChild$")
	command.Env = []string{"LANG=C.UTF-8", "SFL_TEST_READER_SIGNAL=1"}
	command.Stdin, command.Stdout = input, output
	if command.Start() != nil {
		t.Fatal("signal child start")
	}
	input.Close()
	output.Close()
	p := &nativeGuestProcess{in: write, out: read, command: command, group: group, groupFD: fd, done: make(chan struct{})}
	go func() { p.waitErr = command.Wait(); close(p.done) }()
	defer func() { command.Process.Kill(); <-p.done; write.Close(); fd.Close() }()
	if line, err := bufio.NewReader(read).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatal("signal handler not ready", err)
	}
	cleanup, done := context.WithTimeout(context.Background(), 2*time.Second)
	defer done()
	if p.Stop(cleanup) != nil || cleanup.Err() != nil {
		t.Fatal("idle reader consumed its entire cleanup deadline")
	}
	if raw, err := os.ReadFile(filepath.Join(group, "cgroup.kill")); err != nil || string(raw) != "1" {
		t.Fatal("cooperative exit replaced physical-stop enforcement")
	}
}
