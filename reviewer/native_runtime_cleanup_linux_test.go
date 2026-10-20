//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeRuntimeHostRecoveryRemovesRecordedTreeAfterPreparerDisappears(t *testing.T) {
	m, body := nativeRuntimeFixture(t)
	parent := nativeRuntimePrivateParent(t)
	binding := authTestBinding()
	hash := strings.Repeat("d", 64)
	r, err := prepareNativeRuntimeOwned(parent, bytes.NewReader(nativeRuntimeFrame(t, m, body)), m.Program, bytes.NewReader([]byte("synthetic QEMU pin")), func(r *nativeRuntimeRoot) error {
		return r.recordOwnership(binding, hash)
	})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate losing the preparer's process: no Close callback runs.
	r.root.Close()
	r.parent.Close()
	reopened, err := os.OpenRoot(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := removeNativeRuntimeOwned(reopened, binding, hash); err != nil {
		t.Fatal(err)
	}
	if err := removeNativeRuntimeOwned(reopened, binding, hash); err != nil {
		t.Fatal("retry", err)
	}
	if _, err := reopened.Lstat(nativeRuntimeDirectory); !os.IsNotExist(err) {
		t.Fatal("runtime retained", err)
	}
}

func TestNativeRuntimeRecoveryPreservesUnownedOrReplacedTrees(t *testing.T) {
	for _, kind := range []string{"unowned", "binding", "manifest", "replacement"} {
		t.Run(kind, func(t *testing.T) {
			parent := nativeRuntimePrivateParent(t)
			path := filepath.Join(parent, nativeRuntimeDirectory)
			if os.Mkdir(path, 0700) != nil {
				t.Fatal("setup")
			}
			root, err := os.OpenRoot(parent)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			info, err := root.Lstat(nativeRuntimeDirectory)
			if err != nil {
				t.Fatal(err)
			}
			binding, hash := authTestBinding(), strings.Repeat("d", 64)
			if kind != "unowned" {
				owned := &nativeRuntimeRoot{parent: root, identity: info}
				if owned.recordOwnership(binding, hash) != nil {
					t.Fatal("record")
				}
			}
			if kind == "binding" {
				binding.LaunchID = strings.Repeat("f", 32)
			}
			if kind == "manifest" {
				hash = strings.Repeat("e", 64)
			}
			if kind == "replacement" {
				if os.Rename(path, path+"-original") != nil || os.Mkdir(path, 0700) != nil {
					t.Fatal("replace")
				}
			}
			marker := filepath.Join(path, "preserve")
			if os.WriteFile(marker, []byte("unrelated"), 0600) != nil {
				t.Fatal("marker")
			}
			if removeNativeRuntimeOwned(root, binding, hash) == nil {
				t.Fatal("unsafe cleanup accepted")
			}
			if raw, err := os.ReadFile(marker); err != nil || string(raw) != "unrelated" {
				t.Fatal("unrelated tree changed", err)
			}
		})
	}
}

func TestNativeRuntimeSupervisorRefusesCleanupUntilWholeUnitStop(t *testing.T) {
	p, manager := nativeHostPhaseFixture(t)
	ctx := context.Background()
	if p.supervisor.CleanupRuntime(ctx, p.request.Binding) == nil {
		t.Fatal("live cleanup accepted")
	}
	if p.supervisor.AbortAndWait(ctx, p.request.Binding) != nil {
		t.Fatal("stop")
	}
	if p.supervisor.CleanupRuntime(ctx, p.request.Binding) != nil {
		t.Fatal("stopped empty cleanup refused")
	}
	manager.kernel.mu.Lock()
	manager.kernel.populated = true
	manager.kernel.mu.Unlock()
	if p.supervisor.CleanupRuntime(ctx, p.request.Binding) == nil {
		t.Fatal("nonempty unit cleanup accepted")
	}
}
