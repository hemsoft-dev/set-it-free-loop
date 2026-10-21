//go:build linux

package reviewer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func buildGitAcquisitionTestCLI(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	path := filepath.Join(t.TempDir(), "gitacquire")
	command := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin/go"), "build", "-o", path, "./cmd/gitacquire")
	command.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOWORK=off", "CGO_ENABLED=0", "GOFLAGS=")
	if raw, err := command.CombinedOutput(); err != nil {
		t.Fatal("build actual fixed CLI", err, string(raw))
	}
	return path
}

func TestGitAcquisitionFixedCLIRejectsBeforeOwningUninheritedFDs(t *testing.T) {
	program := buildGitAcquisitionTestCLI(t)
	binding := gitMaterializationBinding{strings.Repeat("a", 32), Digest("release"), Digest("runtime"), testJob().Snapshot}
	raw, err := jsonMarshalGitBinding(binding)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"empty", "wrong-mode", "malformed", "oversized", "noncanonical", "wrong-hash", "noncanonical-pid", "missing-root-fds", "missing-worker-fds", "missing-invocation"} {
		t.Run(kind, func(t *testing.T) {
			args := []string{"/missing/bootstrap.sock", raw, Digest(binding), strconv.Itoa(os.Getpid())}
			env := []string{"INVOCATION_ID=" + strings.Repeat("b", 32)}
			switch kind {
			case "empty":
				args = nil
			case "wrong-mode":
				args = []string{"--other", raw, Digest(binding)}
			case "malformed":
				args[1] = "{}"
			case "oversized":
				args[1] = strings.Repeat(" ", 4097) + raw
			case "noncanonical":
				args[1] = " " + raw
			case "wrong-hash":
				args[2] = Digest("different binding")
			case "noncanonical-pid":
				args[3] = "0" + args[3]
			case "missing-worker-fds":
				args = []string{"--worker", raw, Digest(binding)}
			case "missing-invocation":
				env = nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, program, args...)
			command.Env = env
			command.Dir = t.TempDir()
			output, err := command.CombinedOutput()
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ExitCode() != 1 || len(output) != 0 {
				t.Fatal("CLI rejection or runtime descriptor safety", err, string(output))
			}
			entries, err := os.ReadDir(command.Dir)
			if err != nil || len(entries) != 0 {
				t.Fatal("rejected CLI wrote state", err)
			}
		})
	}
}

func TestGitWorkerNamespaceFixedCapabilitiesAndFDConsumption(t *testing.T) {
	binding := gitMaterializationBinding{strings.Repeat("a", 32), Digest("release"), Digest("runtime"), testJob().Snapshot}
	args, err := gitWorkerNamespaceArgs(binding, strings.Repeat("b", 32))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, required := range []string{"--unshare-all --unshare-user --disable-userns", "--cap-drop ALL --clearenv", "--ro-bind-fd 8 /", "--ro-bind-fd 9 /broker", "--bind-fd 10 /work", "--bind-fd 11 /tmp", "--proc /proc --remount-ro /proc", "--seccomp 7", "-- /proc/self/fd/4 --worker"} {
		if !strings.Contains(joined, required) {
			t.Fatal("missing fixed boundary", required)
		}
	}
	for _, forbidden := range []string{"--share-net", "--unshare-user-try", "--dev ", "--tmpfs", "--bind-try", "--ro-bind-try", "--cap-add", "/bin/sh"} {
		if strings.Contains(joined, forbidden) {
			t.Fatal("forbidden boundary fallback", forbidden)
		}
	}
	decoded, err := parseGitProcessBinding(args[len(args)-2], args[len(args)-1])
	if err != nil || decoded != binding {
		t.Fatal("actual process binding frame", err)
	}
	if _, err := gitWorkerNamespaceArgs(binding, "wrong"); err == nil {
		t.Fatal("invalid invocation accepted")
	}
}
