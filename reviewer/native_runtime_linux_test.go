//go:build linux

package reviewer

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func nativeRuntimeFixture(t *testing.T) (nativeRuntimeManifest, []byte) {
	t.Helper()
	program := VMAssetPin{SHA256: authHash([]byte("synthetic QEMU pin")), Bytes: 18}
	manifest := nativeRuntimeManifest{Version: 1, Purpose: "native-qemu-runtime", Program: program}
	var data bytes.Buffer
	for _, path := range []string{"firmware/kvmvapic.bin", "firmware/linuxboot.bin", "firmware/linuxboot_dma.bin", "lib64/ld-linux-x86-64.so.2", "usr/lib/x86_64-linux-gnu/libc.so.6"} {
		raw := []byte("fixture bytes for " + path)
		manifest.Files = append(manifest.Files, nativeRuntimeFile{path, authHash(raw), int64(len(raw))})
		data.Write(raw)
	}
	return manifest, data.Bytes()
}
func nativeRuntimeFrame(t *testing.T, m nativeRuntimeManifest, body []byte) []byte {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	out.WriteString(nativeRuntimeMagic)
	binary.Write(&out, binary.BigEndian, uint32(len(raw)))
	out.Write(raw)
	out.Write(body)
	return out.Bytes()
}
func TestNativeRuntimeBindsCanonicalManifestAndExactFileBodies(t *testing.T) {
	m, body := nativeRuntimeFixture(t)
	frame := nativeRuntimeFrame(t, m, body)
	calls := 0
	consume := func(file nativeRuntimeFile, input io.Reader) error {
		calls++
		raw, err := io.ReadAll(input)
		if err != nil || authHash(raw) != file.SHA256 {
			t.Fatal("wrong body", err)
		}
		return nil
	}
	got, err := streamNativeRuntime(bytes.NewReader(frame), m.Program, consume)
	if err != nil || calls != len(m.Files) || Digest(got) != Digest(m) {
		t.Fatal("valid frame rejected", err, calls)
	}
	for _, invalid := range [][]byte{frame[:len(frame)-1], append(append([]byte{}, frame...), 0), append([]byte("wrong"), frame...)} {
		if _, err := streamNativeRuntime(bytes.NewReader(invalid), m.Program, func(_ nativeRuntimeFile, r io.Reader) error { _, e := io.Copy(io.Discard, r); return e }); err == nil {
			t.Fatal("invalid framing accepted")
		}
	}
	corrupt := append([]byte{}, frame...)
	corrupt[len(corrupt)-1] ^= 1
	if _, err := streamNativeRuntime(bytes.NewReader(corrupt), m.Program, func(_ nativeRuntimeFile, r io.Reader) error { _, e := io.Copy(io.Discard, r); return e }); err == nil {
		t.Fatal("corrupt body accepted")
	}
	if _, err := streamNativeRuntime(bytes.NewReader(frame), VMAssetPin{SHA256: strings.Repeat("a", 64), Bytes: m.Program.Bytes}, consume); err == nil {
		t.Fatal("foreign QEMU accepted")
	}
	if _, err := streamNativeRuntime(bytes.NewReader(frame), m.Program, func(nativeRuntimeFile, io.Reader) error { return nil }); err == nil {
		t.Fatal("partially consumed body accepted")
	}
}
func TestNativeRuntimeRejectsUnsafeMetadataBeforeFileCallback(t *testing.T) {
	original, body := nativeRuntimeFixture(t)
	for _, change := range []func(*nativeRuntimeManifest){
		func(m *nativeRuntimeManifest) { m.Version++ },
		func(m *nativeRuntimeManifest) { m.Files[0].Path = "../outside" },
		func(m *nativeRuntimeManifest) { m.Files[0].Path = "/lib64/ld-linux-x86-64.so.2" },
		func(m *nativeRuntimeManifest) { m.Files[0].Path = "usr/lib/x86_64-linux-gnu/sub/libc.so.6" },
		func(m *nativeRuntimeManifest) { m.Files[1].Path = m.Files[0].Path },
		func(m *nativeRuntimeManifest) { m.Files[0].Bytes = 65 << 20 },
		func(m *nativeRuntimeManifest) { m.Files[0].SHA256 = "invalid" },
		func(m *nativeRuntimeManifest) { m.Files = m.Files[:4] },
	} {
		m := original
		m.Files = append([]nativeRuntimeFile{}, original.Files...)
		change(&m)
		calls := 0
		_, err := streamNativeRuntime(bytes.NewReader(nativeRuntimeFrame(t, m, body)), original.Program, func(nativeRuntimeFile, io.Reader) error { calls++; return nil })
		if err == nil || calls != 0 {
			t.Fatal("unsafe metadata reached callback", err, calls)
		}
	}
}
