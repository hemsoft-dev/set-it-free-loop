//go:build linux

package reviewer

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
)

var ErrNativeRuntime = errors.New("native QEMU runtime artifact is invalid")

const nativeRuntimeMagic = "SFL-QEMU-RUNTIME-1\n"
const nativeRuntimeManifestLimit = 32 << 10
const nativeRuntimeBytes = 128 << 20

// The artifact contains only named regular-file bytes. It has no archive entry
// types, symlinks, ownership, executable selector or extraction command.
type nativeRuntimeFile struct {
	Path, SHA256 string
	Bytes        int64
}

type nativeRuntimeManifest struct {
	Version int
	Purpose string
	Program VMAssetPin
	Files   []nativeRuntimeFile
}

var nativeRuntimeLibrary = regexp.MustCompile(`^lib[A-Za-z0-9_+.-]+\.so(?:\.[A-Za-z0-9_-]+)*$`)

func nativeRuntimePath(path string) bool {
	switch path {
	case "lib64/ld-linux-x86-64.so.2", "firmware/linuxboot.bin", "firmware/linuxboot_dma.bin", "firmware/kvmvapic.bin":
		return true
	}
	for _, prefix := range []string{"lib/x86_64-linux-gnu/", "usr/lib/x86_64-linux-gnu/"} {
		if strings.HasPrefix(path, prefix) {
			return nativeRuntimeLibrary.MatchString(strings.TrimPrefix(path, prefix))
		}
	}
	return false
}

func (m nativeRuntimeManifest) valid(program VMAssetPin) bool {
	if m.Version != 1 || m.Purpose != "native-qemu-runtime" || m.Program != program || !validAuthHash(program.SHA256) || program.Bytes <= 0 || program.Bytes > 64<<20 || len(m.Files) < 5 || len(m.Files) > 128 {
		return false
	}
	var total int64
	previous := ""
	seen := map[string]bool{}
	for _, file := range m.Files {
		if !nativeRuntimePath(file.Path) || file.Path <= previous || !validAuthHash(file.SHA256) || file.Bytes <= 0 || file.Bytes > 64<<20 {
			return false
		}
		total += file.Bytes
		if total > nativeRuntimeBytes {
			return false
		}
		previous = file.Path
		seen[file.Path] = true
	}
	return seen["lib64/ld-linux-x86-64.so.2"] && seen["firmware/linuxboot.bin"] && seen["firmware/linuxboot_dma.bin"] && seen["firmware/kvmvapic.bin"] && (seen["lib/x86_64-linux-gnu/libc.so.6"] || seen["usr/lib/x86_64-linux-gnu/libc.so.6"])
}

// streamNativeRuntime accepts only canonical metadata and exactly its bounded,
// individually hashed file bodies. Callers must finish validation before exec;
// a callback error or invalid later body does not qualify a prepared filesystem.
func streamNativeRuntime(input io.Reader, program VMAssetPin, consume func(nativeRuntimeFile, io.Reader) error) (nativeRuntimeManifest, error) {
	var empty nativeRuntimeManifest
	if input == nil || consume == nil {
		return empty, ErrNativeRuntime
	}
	limited := io.LimitReader(input, nativeRuntimeBytes+nativeRuntimeManifestLimit+int64(len(nativeRuntimeMagic))+5)
	magic := make([]byte, len(nativeRuntimeMagic))
	if _, err := io.ReadFull(limited, magic); err != nil || string(magic) != nativeRuntimeMagic {
		return empty, ErrNativeRuntime
	}
	var size uint32
	if binary.Read(limited, binary.BigEndian, &size) != nil || size == 0 || size > nativeRuntimeManifestLimit {
		return empty, ErrNativeRuntime
	}
	raw := make([]byte, int(size))
	if _, err := io.ReadFull(limited, raw); err != nil {
		return empty, ErrNativeRuntime
	}
	manifest, err := DecodeStrict[nativeRuntimeManifest](raw, nativeRuntimeManifestLimit)
	if err != nil || !manifest.valid(program) {
		return empty, ErrNativeRuntime
	}
	canonical, _ := json.Marshal(manifest)
	if !bytes.Equal(raw, canonical) {
		return empty, ErrNativeRuntime
	}
	for _, file := range manifest.Files {
		body := &io.LimitedReader{R: limited, N: file.Bytes}
		hash := sha256.New()
		if consume(file, io.TeeReader(body, hash)) != nil || body.N != 0 || hex.EncodeToString(hash.Sum(nil)) != file.SHA256 {
			return empty, ErrNativeRuntime
		}
	}
	var trailing [1]byte
	if n, err := limited.Read(trailing[:]); n != 0 || err != io.EOF {
		return empty, ErrNativeRuntime
	}
	return manifest, nil
}
