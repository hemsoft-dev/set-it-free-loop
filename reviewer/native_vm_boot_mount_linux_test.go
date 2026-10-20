//go:build linux

package reviewer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNativeBootMountCreatesAbsentTargetBeforeKernelMount(t *testing.T) {
	target := filepath.Join(t.TempDir(), "sys")
	calls := 0
	mount := func(source, gotTarget, kind string, flags uintptr, options string) error {
		calls++
		info, err := os.Lstat(gotTarget)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0755 {
			t.Errorf("mount target absent or unsafe at kernel mount: %v", err)
			return ErrVMBoot
		}
		if source != "sysfs" || kind != "sysfs" || gotTarget != target || flags != 7 || options != "" {
			t.Fatal("mount arguments changed")
		}
		return nil
	}
	if err := nativeGuestMount("sysfs", target, "sysfs", 7, "", mount); err != nil || calls != 1 {
		t.Fatal("bootstrap mount failed", err, calls)
	}
	if err := nativeGuestMount("sysfs", target, "sysfs", 7, "", mount); err != nil || calls != 2 {
		t.Fatal("existing directory refused", err, calls)
	}
}

func TestNativeBootMountRejectsUnsafeTargetsBeforeKernelMount(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if os.WriteFile(file, nil, 0600) != nil {
		t.Fatal("fixture setup")
	}
	link := filepath.Join(root, "link")
	if os.Symlink(root, link) != nil {
		t.Fatal("fixture setup")
	}
	writable := filepath.Join(root, "writable")
	if os.Mkdir(writable, 0777) != nil || os.Chmod(writable, 0777) != nil {
		t.Fatal("fixture setup")
	}
	for _, target := range []string{file, link, writable, filepath.Join(root, "absent", "sys")} {
		calls := 0
		mount := func(string, string, string, uintptr, string) error { calls++; return nil }
		if err := nativeGuestMount("sysfs", target, "sysfs", 0, "", mount); err == nil || calls != 0 {
			t.Error("unsafe target reached kernel mount", target, err, calls)
		}
	}
}
