//go:build linux

package reviewer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

const nativeRuntimeHelperFD = 12
const nativeRuntimeSerialFD = 13
const nativeRuntimeRootFD = 14

// The original root-owned helper inode is a trusted host bootstrap dependency,
// not a sealed payload file. Retaining its open descriptor preserves its actual
// execution context across pathname replacement. Its host interpreter/libraries
// still require qualification; this function does not attest those dependencies.
func retainNativeBootstrap(ctx context.Context, asset VMBootAsset) (*os.File, error) {
	if ctx == nil || !filepath.IsAbs(asset.Path) || filepath.Clean(asset.Path) != asset.Path || !validAuthHash(asset.SHA256) || asset.Bytes <= 0 || asset.Bytes > 64<<20 {
		return nil, ErrNativeRuntime
	}
	file, err := os.OpenFile(asset.Path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrNativeRuntime
	}
	good := false
	defer func() {
		if !good {
			file.Close()
		}
	}()
	if err := verifyNativeBootstrap(ctx, file, VMAssetPin{asset.SHA256, asset.Bytes}); err != nil {
		return nil, err
	}
	good = true
	return file, nil
}
func verifyNativeBootstrap(ctx context.Context, file *os.File, pin VMAssetPin) error {
	if ctx == nil || file == nil || !validAuthHash(pin.SHA256) || pin.Bytes <= 0 || pin.Bytes > 64<<20 {
		return ErrNativeRuntime
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != pin.Bytes || info.Mode().Perm()&0022 != 0 || info.Mode().Perm()&0111 == 0 {
		return ErrNativeRuntime
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 || st.Nlink != 1 {
		return ErrNativeRuntime
	}
	input := io.NewSectionReader(file, 0, pin.Bytes+1)
	hash := sha256.New()
	buffer := make([]byte, 32<<10)
	var size int64
	for {
		if ctx.Err() != nil {
			return errors.Join(ErrNativeRuntime, ctx.Err())
		}
		n, err := input.Read(buffer)
		if n > 0 {
			size += int64(n)
			hash.Write(buffer[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil || size > pin.Bytes {
			return ErrNativeRuntime
		}
	}
	if size != pin.Bytes || hex.EncodeToString(hash.Sum(nil)) != pin.SHA256 {
		return ErrNativeRuntime
	}
	return nil
}

// All namespace policy and descriptor positions are trusted fixed code. The
// helper consumes and closes the mount-source descriptor before QEMU executes;
// carrying a host directory descriptor into QEMU could escape the new root.
func nativeRuntimeNamespaceArgs(qemuArgs []string) []string {
	result := []string{"--unshare-all", "--unshare-user", "--disable-userns", "--die-with-parent", "--new-session", "--clearenv", "--setenv", "LANG", "C.UTF-8", "--ro-bind-fd", strconv.Itoa(nativeRuntimeRootFD), "/", "--proc", "/proc", "--dev", "/dev", "--size", "16777216", "--tmpfs", "/tmp", "--chdir", "/", "--argv0", "sfl-native-qemu", "--", "/qemu"}
	return append(result, qemuArgs...)
}
