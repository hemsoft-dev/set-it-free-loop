//go:build linux

package reviewer

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
)

// Trusted source release inputs only. No provider binding, credential, unit,
// arbitrary executable selector or serialized admission enters this contract.
// The QEMU runtime file format is shared plain pinned bytes, not a native
// provider manifest or an auth/stop receipt.
type gitVMAssetSpec struct {
	Manifest                                                 gitVMManifest
	QEMU, Kernel, Initrd, BIOS, QEMURuntime, NamespaceHelper VMBootAsset
	BootstrapLibraries                                       []VMBootAsset
}

func gitVMAssetPin(a VMBootAsset) VMAssetPin { return VMAssetPin{a.SHA256, a.Bytes} }
func (s gitVMAssetSpec) valid() bool {
	if !s.Manifest.valid() {
		return false
	}
	assets := []VMBootAsset{s.QEMU, s.Kernel, s.Initrd, s.BIOS, s.QEMURuntime, s.NamespaceHelper}
	pins := []VMAssetPin{s.Manifest.QEMU, s.Manifest.Kernel, s.Manifest.Initrd, s.Manifest.BIOS, s.Manifest.QEMURuntime, s.Manifest.NamespaceHelper}
	for i, a := range assets {
		if !sourcePath(a.Path) || gitVMAssetPin(a) != pins[i] {
			return false
		}
	}
	libraries, err := nativeBootstrapLibraryPins(s.BootstrapLibraries)
	if err != nil || len(libraries) != len(s.Manifest.BootstrapLibraries) {
		return false
	}
	for i, pin := range libraries {
		if pin != s.Manifest.BootstrapLibraries[i] {
			return false
		}
	}
	return true
}

// Owns only immutable boot inputs and the original root-owned namespace helper.
// Opening does not touch KVM, create a unit/channel/VM, promote a release or
// confer source publication authority. The future supervisor owns each returned
// launch duplicate independently through complete original-instance shutdown.
type gitVMAssets struct {
	mu       sync.Mutex
	manifest gitVMManifest
	files    [6]*os.File // QEMU, kernel, initrd, BIOS, runtime artifact, original helper
	closed   bool
}

func openGitVMAssets(ctx context.Context, spec gitVMAssetSpec) (_ *gitVMAssets, result error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, ErrVMBoot
	}
	// Retain metadata before any bounded IO; caller mutation after return cannot
	// substitute a library list in a later launch check.
	spec.Manifest.BootstrapLibraries = append([]nativeRuntimeFile(nil), spec.Manifest.BootstrapLibraries...)
	spec.BootstrapLibraries = append([]VMBootAsset(nil), spec.BootstrapLibraries...)
	if !spec.valid() || verifyNativeBootstrapLibraries(ctx, spec.Manifest.BootstrapLibraries) != nil {
		return nil, ErrVMBoot
	}
	a := &gitVMAssets{manifest: spec.Manifest}
	defer func() {
		if result != nil {
			a.Close()
		}
	}()
	for i, input := range []VMBootAsset{spec.QEMU, spec.Kernel, spec.Initrd, spec.BIOS, spec.QEMURuntime} {
		file, err := sealVMAsset(ctx, input, i == 0)
		if err != nil {
			return nil, fmt.Errorf("source VM asset seal role %d: %w", i, ErrVMBoot)
		}
		a.files[i] = file
	}
	helper, err := retainNativeBootstrap(ctx, spec.NamespaceHelper)
	if err != nil {
		return nil, ErrVMBoot
	}
	a.files[5] = helper
	// Pinning the outer artifact also requires its complete canonical inner frame
	// to bind this exact QEMU program and every dependency byte.
	if err := a.check(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

type gitVMAssetReader struct {
	ctx context.Context
	io.Reader
}

func (r gitVMAssetReader) Read(raw []byte) (int, error) {
	if r.ctx.Err() != nil {
		return 0, r.ctx.Err()
	}
	return r.Reader.Read(raw)
}

// Caller holds the asset lock. Section readers never change shared offsets.
func (a *gitVMAssets) check(ctx context.Context) error {
	if ctx == nil || ctx.Err() != nil || a.closed || !a.manifest.valid() {
		return ErrVMBoot
	}
	pins := []VMAssetPin{a.manifest.QEMU, a.manifest.Kernel, a.manifest.Initrd, a.manifest.BIOS, a.manifest.QEMURuntime}
	for i, pin := range pins {
		if verifyGrantedVMAsset(ctx, a.files[i], pin, i) != nil {
			return fmt.Errorf("source VM asset retained role %d: %w", i, ErrVMBoot)
		}
	}
	if verifyNativeBootstrap(ctx, a.files[5], a.manifest.NamespaceHelper) != nil || verifyNativeBootstrapLibraries(ctx, a.manifest.BootstrapLibraries) != nil {
		return ErrVMBoot
	}
	_, err := streamNativeRuntime(gitVMAssetReader{ctx, io.NewSectionReader(a.files[4], 0, a.manifest.QEMURuntime.Bytes)}, a.manifest.QEMU, func(_ nativeRuntimeFile, body io.Reader) error {
		_, err := io.Copy(io.Discard, gitVMAssetReader{ctx, body})
		return err
	})
	if err != nil || ctx.Err() != nil {
		return fmt.Errorf("source VM asset runtime frame: %w", ErrVMBoot)
	}
	return nil
}

func (a *gitVMAssets) duplicate(ctx context.Context, q gitVMRequest) (_ []*os.File, result error) {
	if a == nil || ctx == nil || !q.valid() {
		return nil, ErrVMBoot
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if Digest(q.Manifest) != Digest(a.manifest) || a.check(ctx) != nil {
		return nil, ErrVMBoot
	}
	files := make([]*os.File, 0, len(a.files))
	defer func() {
		if result != nil {
			closeVMFiles(files)
		}
	}()
	for _, original := range a.files {
		duplicate, err := duplicateGitControlFile(original)
		if err != nil {
			return nil, ErrVMBoot
		}
		files = append(files, duplicate)
	}
	return files, nil
}

func (a *gitVMAssets) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	a.closed = true
	return closeVMFiles(a.files[:])
}
