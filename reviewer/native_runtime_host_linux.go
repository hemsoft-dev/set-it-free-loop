//go:build linux

package reviewer

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// NativeVMRuntimeSpec is trusted release configuration, never model/job input.
// The artifact is sealed payload authority. The original namespace helper and
// named host library pins are separately checked bootstrap prerequisites.
const nativeRuntimeArtifactLimit int64 = nativeRuntimeBytes + nativeRuntimeManifestLimit + int64(len(nativeRuntimeMagic)) + 4

type NativeVMRuntimeSpec struct {
	Artifact           VMBootAsset
	NamespaceHelper    VMBootAsset
	BootstrapLibraries []VMBootAsset
}

func nativeBootstrapLibraryPins(assets []VMBootAsset) ([]nativeRuntimeFile, error) {
	if len(assets) < 2 || len(assets) > 32 {
		return nil, ErrNativeRuntime
	}
	pins := make([]nativeRuntimeFile, 0, len(assets))
	previous := ""
	loader, libc := false, false
	for _, asset := range assets {
		if !filepath.IsAbs(asset.Path) || filepath.Clean(asset.Path) != asset.Path || !validAuthHash(asset.SHA256) || asset.Bytes <= 0 || asset.Bytes > 64<<20 {
			return nil, ErrNativeRuntime
		}
		path := strings.TrimPrefix(asset.Path, "/")
		if !nativeRuntimePath(path) || strings.HasPrefix(path, "firmware/") || path <= previous {
			return nil, ErrNativeRuntime
		}
		previous = path
		loader = loader || path == "lib64/ld-linux-x86-64.so.2"
		libc = libc || path == "lib/x86_64-linux-gnu/libc.so.6" || path == "usr/lib/x86_64-linux-gnu/libc.so.6"
		pins = append(pins, nativeRuntimeFile{path, asset.SHA256, asset.Bytes})
	}
	if !loader || !libc {
		return nil, ErrNativeRuntime
	}
	return pins, nil
}
func nativeBootstrapPinsValid(pins []nativeRuntimeFile) bool {
	assets := make([]VMBootAsset, 0, len(pins))
	for _, pin := range pins {
		assets = append(assets, VMBootAsset{"/" + pin.Path, pin.SHA256, pin.Bytes})
	}
	_, err := nativeBootstrapLibraryPins(assets)
	return err == nil
}
func verifyNativeBootstrapLibraries(ctx context.Context, pins []nativeRuntimeFile) error {
	if ctx == nil || !nativeBootstrapPinsValid(pins) {
		return ErrNativeRuntime
	}
	for _, pin := range pins {
		resolved, err := filepath.EvalSymlinks("/" + pin.Path)
		if err != nil || (!strings.HasPrefix(resolved, "/lib/") && !strings.HasPrefix(resolved, "/lib64/") && !strings.HasPrefix(resolved, "/usr/lib/")) {
			return ErrNativeRuntime
		}
		file, err := os.OpenFile(resolved, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
		if err != nil {
			return ErrNativeRuntime
		}
		err = verifyNativeRootOwnedFile(ctx, file, VMAssetPin{pin.SHA256, pin.Bytes}, false)
		file.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (i *VMBootImages) duplicateNativeRuntime(ctx context.Context) ([]*os.File, error) {
	if i == nil {
		return nil, ErrNativeRuntime
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed || i.runtimeArtifact == nil || i.namespaceHelper == nil {
		return nil, ErrNativeRuntime
	}
	if verifyGrantedVMAsset(ctx, i.runtimeArtifact, VMAssetPin{i.runtimeAsset.SHA256, i.runtimeAsset.Bytes}, 4) != nil || verifyNativeBootstrap(ctx, i.namespaceHelper, VMAssetPin{i.namespaceHelperAsset.SHA256, i.namespaceHelperAsset.Bytes}) != nil || verifyNativeBootstrapLibraries(ctx, i.bootstrapLibraries) != nil {
		return nil, ErrNativeRuntime
	}
	files := []*os.File{}
	for _, original := range []*os.File{i.runtimeArtifact, i.namespaceHelper} {
		fd, err := unix.FcntlInt(original.Fd(), unix.F_DUPFD_CLOEXEC, 0)
		if err != nil {
			closeVMFiles(files)
			return nil, ErrNativeRuntime
		}
		files = append(files, os.NewFile(uintptr(fd), "native-runtime-grant"))
	}
	return files, nil
}
