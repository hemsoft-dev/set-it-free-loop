//go:build linux

package reviewer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func nativeGuestStopRoutingFixture(t *testing.T, events string) (*nativeGuestProcess, string) {
	t.Helper()
	root := t.TempDir()
	group := filepath.Join(root, "provider")
	if err := os.Mkdir(group, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"cgroup.kill": "0", "cgroup.events": events} {
		if err := os.WriteFile(filepath.Join(group, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	fd, err := os.Open(group)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fd.Close() })
	input, err := os.Create(filepath.Join(root, "owned-input"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { input.Close() })
	done := make(chan struct{})
	close(done)
	return &nativeGuestProcess{in: input, group: group, groupFD: fd, done: done}, root
}
func TestNativeGuestStopUsesOriginalGroupForKillAndEvidence(t *testing.T) {
	for _, populated := range []bool{false, true} {
		t.Run(map[bool]string{false: "original-empty-replacement-populated", true: "original-populated-replacement-empty"}[populated], func(t *testing.T) {
			originalEvents, replacementEvents := "populated 0\n", "populated 1\n"
			if populated {
				originalEvents, replacementEvents = replacementEvents, originalEvents
			}
			process, root := nativeGuestStopRoutingFixture(t, originalEvents)
			old := filepath.Join(root, "retained-original")
			if err := os.Rename(process.group, old); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(process.group, 0700); err != nil {
				t.Fatal(err)
			}
			for name, value := range map[string]string{"cgroup.kill": "0", "cgroup.events": replacementEvents} {
				if err := os.WriteFile(filepath.Join(process.group, name), []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			err := process.Stop(ctx)
			if (err != nil) != populated {
				t.Fatalf("wrong original stop proof: %v", err)
			}
			original, _ := os.ReadFile(filepath.Join(old, "cgroup.kill"))
			replacement, _ := os.ReadFile(filepath.Join(process.group, "cgroup.kill"))
			if string(original) != "1" || string(replacement) != "0" {
				t.Fatalf("wrong cgroup received termination: original=%q replacement=%q", original, replacement)
			}
		})
	}
}
func TestNativeGuestStopRefusesControlSymlinkAndClosedDescriptor(t *testing.T) {
	for _, kind := range []string{"symlink", "closed"} {
		t.Run(kind, func(t *testing.T) {
			process, root := nativeGuestStopRoutingFixture(t, "populated 0\n")
			target := filepath.Join(root, "unrelated")
			if err := os.WriteFile(target, []byte("0"), 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "symlink" {
				if err := os.Remove(filepath.Join(process.group, "cgroup.kill")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(process.group, "cgroup.kill")); err != nil {
					t.Fatal(err)
				}
			} else {
				process.groupFD.Close()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			if err := process.Stop(ctx); err == nil {
				t.Fatal("uncertain descriptor accepted as stopped")
			}
			data, _ := os.ReadFile(target)
			if string(data) != "0" {
				t.Fatal("unrelated file mutated", string(data))
			}
		})
	}
}
