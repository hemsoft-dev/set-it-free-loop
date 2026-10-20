//go:build linux

package reviewer

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNativeBootDiagnosticsBoundedFixedStages(t *testing.T) {
	d := &nativeBootDiagnostics{}
	for _, chunk := range []string{"private value SFL_BOOT_STA", "GE=runtime\r", "\nSFL_BOOT_STAGE=ports\n", "SFL_BOOT_STAGE=unknown\nOut of memory\nKernel panic\n"} {
		if n, err := d.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatal(n, err)
		}
	}
	if got := d.stages(true); !reflect.DeepEqual(got, []string{"diag-ports", "diag-oom", "diag-panic"}) {
		t.Fatal(got)
	}
	if d.tail != nil {
		t.Fatal("transient input retained after finalization")
	}
	d = &nativeBootDiagnostics{}
	d.Write([]byte(strings.Repeat("x", (1<<20)+100)))
	d.Write([]byte("SFL_BOOT_STAGE=ready\n"))
	if d.scanned != 1<<20 || len(d.tail) > 128 || d.stage != "" {
		t.Fatal("unbounded diagnostic intake")
	}
	if got := d.stages(true); !reflect.DeepEqual(got, []string{"diag-vm-error"}) {
		t.Fatal(got)
	}
	for _, invalid := range []string{"diag-secret", "diag-ready\n", "diag-", "arbitrary input"} {
		if nativeVMGuestStage(invalid) {
			t.Fatal("unknown diagnostic accepted", invalid)
		}
	}
}

func TestNativeBootDiagnosticsOwnedStreamDrainsAndCloses(t *testing.T) {
	parent, child, err := nativeBootDiagnosticChannel()
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	defer child.Close()
	d := &nativeBootDiagnostics{}
	done := make(chan struct{})
	go func() { readNativeBootDiagnostics(parent, d); close(done) }()
	if _, err := child.Write([]byte("SFL_BOOT_STAGE=modules\n")); err != nil {
		t.Fatal(err)
	}
	child.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("owned stream failed to drain EOF")
	}
	if got := d.stages(false); !reflect.DeepEqual(got, []string{"diag-modules"}) {
		t.Fatal(got)
	}
	parent2, child2, err := nativeBootDiagnosticChannel()
	if err != nil {
		t.Fatal(err)
	}
	defer child2.Close()
	done2 := make(chan struct{})
	go func() { readNativeBootDiagnostics(parent2, &nativeBootDiagnostics{}); close(done2) }()
	parent2.Close()
	select {
	case <-done2:
	case <-time.After(time.Second):
		t.Fatal("close failed to cancel owned reader")
	}
}
