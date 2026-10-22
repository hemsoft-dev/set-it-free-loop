//go:build linux

package reviewer

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func gitVMAssetsFixture(t *testing.T) (gitVMAssetSpec, gitVMRequest) {
	t.Helper()
	q := gitVMGuestTestRequest(t).Source
	directory := t.TempDir()
	spec := gitVMAssetSpec{Manifest: q.Manifest}
	spec.QEMU = vmAsset(t, filepath.Join(directory, "qemu"), []byte("synthetic QEMU bytes, never executed"))
	spec.Kernel = vmAsset(t, filepath.Join(directory, "kernel"), []byte("synthetic kernel"))
	spec.Initrd = vmAsset(t, filepath.Join(directory, "initrd"), []byte("synthetic initrd"))
	spec.BIOS = vmAsset(t, filepath.Join(directory, "bios"), []byte("synthetic firmware"))
	m, body := nativeRuntimeFixture(t)
	m.Program = gitVMAssetPin(spec.QEMU)
	spec.QEMURuntime = vmAsset(t, filepath.Join(directory, "qemu-runtime"), nativeRuntimeFrame(t, m, body))
	spec.NamespaceHelper = nativeRootOwnedTestAsset(t, "/usr/bin/bwrap")
	spec.BootstrapLibraries = []VMBootAsset{nativeRootOwnedTestAsset(t, "/lib64/ld-linux-x86-64.so.2"), nativeRootOwnedTestAsset(t, "/usr/lib/x86_64-linux-gnu/libc.so.6")}
	spec.Manifest.QEMU = gitVMAssetPin(spec.QEMU)
	spec.Manifest.Kernel = gitVMAssetPin(spec.Kernel)
	spec.Manifest.Initrd = gitVMAssetPin(spec.Initrd)
	spec.Manifest.BIOS = gitVMAssetPin(spec.BIOS)
	spec.Manifest.QEMURuntime = gitVMAssetPin(spec.QEMURuntime)
	spec.Manifest.NamespaceHelper = gitVMAssetPin(spec.NamespaceHelper)
	var err error
	spec.Manifest.BootstrapLibraries, err = nativeBootstrapLibraryPins(spec.BootstrapLibraries)
	if err != nil {
		t.Fatal(err)
	}
	q.Manifest = spec.Manifest
	q.Manifest.BootstrapLibraries = append([]nativeRuntimeFile(nil), spec.Manifest.BootstrapLibraries...)
	q.Binding.RuntimeHash = Digest(q.Manifest)
	if !q.valid() || !spec.valid() {
		t.Fatal("invalid synthetic source asset fixture")
	}
	return spec, q
}

func TestGitVMAssetsRetainSealedOriginalsAfterInputReplacementAndClose(t *testing.T) {
	spec, q := gitVMAssetsFixture(t)
	a, err := openGitVMAssets(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	original, err := os.ReadFile(spec.Kernel.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(spec.Kernel.Path, spec.Kernel.Path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spec.Kernel.Path, []byte("replacement"), 0500); err != nil {
		t.Fatal(err)
	}
	// The caller owns its metadata slices, independently of the retained owner.
	spec.Manifest.BootstrapLibraries[0].SHA256 = Digest("foreign library")
	spec.BootstrapLibraries[0].SHA256 = Digest("foreign prerequisite")
	files, err := a.duplicate(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	defer closeVMFiles(files)
	if len(files) != 6 {
		t.Fatal("wrong fixed descriptor roles")
	}
	for i, file := range files {
		fd, err := gitControlFileFD(file)
		if err != nil {
			t.Fatal(err)
		}
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err != nil || flags&unix.FD_CLOEXEC == 0 {
			t.Fatal("inheritable launch duplicate")
		}
		if i < 5 {
			seals, err := unix.FcntlInt(uintptr(fd), unix.F_GET_SEALS, 0)
			if err != nil || seals&requiredStubSeals != requiredStubSeals {
				t.Fatal("unsealed boot duplicate")
			}
		}
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(io.NewSectionReader(files[1], 0, int64(len(original))))
	if err != nil || string(data) != string(original) {
		t.Fatal("source replacement changed retained kernel", err)
	}
	if files, err := a.duplicate(context.Background(), q); err == nil {
		closeVMFiles(files)
		t.Fatal("closed asset owner granted more descriptors")
	}
}

func TestGitVMAssetsRefuseForeignPurposeAndRuntimeProgramWithoutLeaks(t *testing.T) {
	spec, q := gitVMAssetsFixture(t)
	before := gitPacketFDCount(t)
	bad := spec
	bad.Manifest.Purpose = "native-review"
	if a, err := openGitVMAssets(context.Background(), bad); err == nil {
		a.Close()
		t.Fatal("provider purpose accepted")
	}
	// A valid outer digest cannot conceal a different inner QEMU program pin.
	m, body := nativeRuntimeFixture(t)
	bad = spec
	bad.QEMURuntime = vmAsset(t, filepath.Join(t.TempDir(), "wrong-runtime"), nativeRuntimeFrame(t, m, body))
	bad.Manifest.QEMURuntime = gitVMAssetPin(bad.QEMURuntime)
	if a, err := openGitVMAssets(context.Background(), bad); err == nil {
		a.Close()
		t.Fatal("foreign runtime program accepted")
	}
	if gitPacketFDCount(t) != before {
		t.Fatal("refused asset opener leaked descriptors")
	}
	a, err := openGitVMAssets(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	foreign := q
	foreign.Manifest.Worker.SHA256 = Digest("other source worker")
	foreign.Binding.RuntimeHash = Digest(foreign.Manifest)
	if !foreign.valid() {
		t.Fatal("invalid foreign request fixture")
	}
	if files, err := a.duplicate(context.Background(), foreign); err == nil {
		closeVMFiles(files)
		t.Fatal("foreign request manifest accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if files, err := a.duplicate(ctx, q); err == nil {
		closeVMFiles(files)
		t.Fatal("cancelled asset grant accepted")
	}
}

// Positive producer coverage is opt-in, local and non-activating. It opens the
// actual source image's outer assets; it never opens KVM or starts a process.
func TestGitVMAssetsActualSourceImageProducer(t *testing.T) {
	directory := os.Getenv("SFL_SOURCE_VM_ASSET_IMAGE")
	if directory == "" {
		t.Skip("explicit model-free source image required")
	}
	spec, q, release := gitVMActualImageFixture(t, directory)
	a, err := openGitVMAssets(context.Background(), spec)
	if err != nil {
		t.Fatal("actual producer assets refused", err)
	}
	defer a.Close()
	files, err := a.duplicate(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	defer closeVMFiles(files)
	t.Logf("retained %d actual source-purpose boot descriptors from source guest producer %s; no VM or auth qualification", len(files), release.SourceHead)
}

func gitVMActualImageFixture(t *testing.T, directory string) (gitVMAssetSpec, gitVMRequest, gitVMGuestRelease) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err != nil || len(raw) > 1<<20 {
		t.Fatal("bounded image metadata required", err)
	}
	var image struct {
		Purpose              string            `json:"purpose"`
		Release              gitVMGuestRelease `json:"release"`
		Kernel, Initrd, BIOS VMAssetPin
		Host                 struct {
			Manifest  struct{ Program VMAssetPin }
			Artifact  VMAssetPin
			Helper    VMBootAsset   `json:"namespace_helper"`
			Libraries []VMBootAsset `json:"bootstrap_dependencies"`
		} `json:"host_runtime"`
	}
	if json.Unmarshal(raw, &image) != nil || image.Purpose != "source-vm-image" || !image.Release.valid() {
		t.Fatal("invalid actual source image")
	}
	asset := func(name string, pin VMAssetPin) VMBootAsset {
		return VMBootAsset{filepath.Join(directory, name), pin.SHA256, pin.Bytes}
	}
	spec := gitVMAssetSpec{QEMU: asset("qemu-program.bin", image.Host.Manifest.Program), Kernel: asset("vmlinuz", image.Kernel), Initrd: asset("source.cpio.gz", image.Initrd), BIOS: asset("bios.bin", image.BIOS), QEMURuntime: asset("host-runtime/native-qemu-runtime.bin", image.Host.Artifact), NamespaceHelper: image.Host.Helper, BootstrapLibraries: image.Host.Libraries}
	q := gitVMGuestTestRequest(t).Source
	spec.Manifest = q.Manifest
	spec.Manifest.QEMU = gitVMAssetPin(spec.QEMU)
	spec.Manifest.Kernel = image.Kernel
	spec.Manifest.Initrd = image.Initrd
	spec.Manifest.BIOS = image.BIOS
	spec.Manifest.QEMURuntime = gitVMAssetPin(spec.QEMURuntime)
	spec.Manifest.NamespaceHelper = gitVMAssetPin(spec.NamespaceHelper)
	spec.Manifest.BootstrapLibraries = image.Release.BootstrapLibraries
	spec.Manifest.Runtime = image.Release.Runtime
	spec.Manifest.Worker = image.Release.Worker
	q.Manifest = spec.Manifest
	q.Binding.RuntimeHash = Digest(spec.Manifest)
	if !spec.valid() {
		t.Fatal("actual source image spec invalid")
	}
	return spec, q, image.Release
}
