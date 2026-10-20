//go:build linux

package reviewer

import (
	"context"
	"encoding/json"
	"os"
	"syscall"
)

// Written before runtime file intake. This contains no auth or model output.
// The host may consume it only after independently proving whole-unit stop.
type nativeRuntimeOwnership struct {
	Version       int
	Binding       AuthBinding
	ManifestHash  string
	Device, Inode uint64
}

func (r *nativeRuntimeRoot) recordOwnership(binding AuthBinding, manifestHash string) error {
	st, ok := r.identity.Sys().(*syscall.Stat_t)
	if !ok || !binding.valid() || !validAuthHash(manifestHash) {
		return ErrNativeRuntime
	}
	raw, err := json.Marshal(nativeRuntimeOwnership{1, binding, manifestHash, uint64(st.Dev), st.Ino})
	if err != nil {
		return ErrNativeRuntime
	}
	return writeAuth(r.parent, "native-runtime.json", raw)
}

// CleanupRuntime can be retried by recovery after AbortAndWait. It never supplies
// stop or auth authority. Missing ownership or a replacement inode is preserved.
func (v *NativeVMSupervisor) CleanupRuntime(ctx context.Context, binding AuthBinding) error {
	if v == nil || v.supervisor == nil || ctx == nil || !binding.valid() {
		return ErrNativeRuntime
	}
	s := v.supervisor
	held, err := s.gate(binding).lock(ctx)
	if err != nil {
		return ErrNativeRuntime
	}
	defer held.close()
	unit, err := s.readUnit(held, binding)
	if err != nil || unit.PayloadKind != "native-vm" || !unit.Aborted || !unit.Stopped {
		return ErrNativeRuntime
	}
	boot, err := s.kernel.BootID()
	if err != nil {
		return ErrNativeRuntime
	}
	if unit.Started && boot == unit.BootID {
		if !unit.Captured || s.kernel.Empty(unit.Cgroup) != nil {
			return ErrNativeRuntime
		}
	}
	return removeNativeRuntimeOwned(held.root, binding, unit.ManifestHash)
}

func removeNativeRuntimeOwned(parent *os.Root, binding AuthBinding, manifestHash string) error {
	info, err := parent.Lstat(nativeRuntimeDirectory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || !info.IsDir() {
		return ErrNativeRuntime
	}
	raw, err := readAuth(parent, "native-runtime.json", false)
	if err != nil {
		return ErrNativeRuntime
	}
	record, err := DecodeStrict[nativeRuntimeOwnership](raw, 4096)
	st, ok := info.Sys().(*syscall.Stat_t)
	if err != nil || !ok || record.Version != 1 || record.Binding != binding || record.ManifestHash != manifestHash || !binding.valid() || !validAuthHash(manifestHash) || record.Inode == 0 || uint64(st.Dev) != record.Device || st.Ino != record.Inode || st.Uid != uint32(os.Geteuid()) {
		return ErrNativeRuntime
	}
	root, err := parent.OpenRoot(nativeRuntimeDirectory)
	if err != nil {
		return ErrNativeRuntime
	}
	// Only fixed directory prefixes can have been hardened by the preparer.
	// os.Root confines chmod/removal even if a stopped tree contains links.
	directories := map[string]bool{}
	for _, path := range []string{".", "proc", "dev", "tmp", "firmware", "lib64", "lib", "lib/x86_64-linux-gnu", "usr", "usr/lib", "usr/lib/x86_64-linux-gnu"} {
		directories[path] = true
	}
	// Duplicate the anchored parent because Close owns both Root handles.
	ownedParent, err := parent.OpenRoot(".")
	if err != nil {
		root.Close()
		return ErrNativeRuntime
	}
	owned := &nativeRuntimeRoot{parent: ownedParent, root: root, identity: info, directories: directories}
	return owned.Close()
}
