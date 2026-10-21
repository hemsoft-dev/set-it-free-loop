//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func gitRuntimeFrame(t *testing.T, m gitRuntimeManifest, bodies map[string][]byte) ([]byte, GitRuntimePins) {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	output.WriteString(gitRuntimeMagic)
	binary.Write(&output, binary.BigEndian, uint32(len(raw)))
	output.Write(raw)
	for _, file := range m.Files {
		output.Write(bodies[file.Path])
	}
	data := output.Bytes()
	return data, GitRuntimePins{m.Git, m.HTTP, VMAssetPin{gitRawHash(data), int64(len(data))}}
}
func gitRuntimeFixture(t *testing.T) (gitRuntimeManifest, map[string][]byte) {
	t.Helper()
	bodies := map[string][]byte{gitRuntimeProgram: []byte("synthetic Git bytes"), gitRuntimeHTTP: []byte("synthetic HTTP helper bytes"), gitRuntimeLoader: []byte("synthetic ELF loader"), "lib/x86_64-linux-gnu/libc.so.6": []byte("synthetic libc")}
	paths := []string{}
	for path := range bodies {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	m := gitRuntimeManifest{Version: 1, Purpose: "isolated-git-acquisition", Files: []nativeRuntimeFile{}}
	for _, path := range paths {
		data := bodies[path]
		file := nativeRuntimeFile{path, gitRawHash(data), int64(len(data))}
		m.Files = append(m.Files, file)
		if path == gitRuntimeProgram {
			m.Git = VMAssetPin{file.SHA256, file.Bytes}
		}
		if path == gitRuntimeHTTP {
			m.HTTP = VMAssetPin{file.SHA256, file.Bytes}
		}
	}
	return m, bodies
}
func actualGitRuntime(t *testing.T) GitRuntimeBuild {
	t.Helper()
	if runtime.GOARCH != "amd64" {
		t.Skip("measured Git runtime currently supports x86-64")
	}
	asset := func(path string) VMBootAsset {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal("installed trusted Git input required", err)
		}
		return VMBootAsset{Path: path, SHA256: gitRawHash(data), Bytes: int64(len(data))}
	}
	build, err := BuildGitRuntime(context.Background(), asset("/usr/bin/git"), asset("/usr/lib/git-core/git-remote-http"))
	if err != nil {
		t.Fatal("actual root-owned ELF producer", err)
	}
	return build
}
func TestGitRuntimeActualELFProducerAndPinnedReadOnlyRoot(t *testing.T) {
	build := actualGitRuntime(t)
	var manifest gitRuntimeManifest
	if json.Unmarshal(build.Manifest, &manifest) != nil || !manifest.valid(build.Pins) || len(manifest.Files) < 8 {
		t.Fatal("missing transitive dependencies")
	}
	root, err := prepareGitRuntime(context.Background(), nativeRuntimePrivateParent(t), bytes.NewReader(build.Artifact), build.Pins, nil)
	if err != nil {
		t.Fatal("actual artifact consumer", err)
	}
	defer root.Close()
	for _, file := range manifest.Files {
		data, err := root.root.ReadFile(file.Path)
		info, statErr := root.root.Stat(file.Path)
		mode := os.FileMode(0444)
		if file.Path == gitRuntimeProgram || file.Path == gitRuntimeHTTP || file.Path == gitRuntimeLoader {
			mode = 0555
		}
		if err != nil || statErr != nil || int64(len(data)) != file.Bytes || gitRawHash(data) != file.SHA256 || info.Mode().Perm() != mode {
			t.Fatal("prepared measured file differs", file.Path, err, statErr)
		}
	}
	fd, err := root.directoryFD()
	if err != nil {
		t.Fatal(err)
	}
	fd.Close()
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := root.parent.Stat(gitRuntimeDirectory); err == nil {
		t.Fatal("closed prepared root retained")
	}
}
func TestGitRuntimeActualProgramsInsidePrivateRoot(t *testing.T) {
	if os.Getenv("SFL_GIT_RUNTIME_NAMESPACE") != "1" {
		t.Skip("explicit offline namespace qualification")
	}
	build := actualGitRuntime(t)
	parent := nativeRuntimePrivateParent(t)
	root, err := prepareGitRuntime(context.Background(), parent, bytes.NewReader(build.Artifact), build.Pins, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, program := range []string{"git", "http"} {
		args := []string{"--unshare-all", "--unshare-user", "--disable-userns", "--die-with-parent", "--new-session", "--clearenv", "--ro-bind", filepath.Join(parent, gitRuntimeDirectory), "/", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/work", "--chdir", "/work", "--setenv", "PATH", "/usr/bin", "--setenv", "GIT_CONFIG_NOSYSTEM", "1", "--setenv", "GIT_CONFIG_GLOBAL", "/dev/null", "--setenv", "GIT_EXEC_PATH", "/usr/lib/git-core", "--"}
		if program == "git" {
			args = append(args, "/usr/bin/git", "--version")
		} else {
			args = append(args, "/usr/lib/git-core/git-remote-http")
		}
		command := exec.Command("/usr/bin/bwrap", args...)
		command.Env = []string{"PATH=/usr/bin:/bin"}
		raw, err := command.CombinedOutput()
		if program == "git" && (err != nil || !strings.HasPrefix(string(raw), "git version ")) {
			t.Fatal("measured Git namespace", string(raw), err)
		}
		if program == "http" && (err == nil || !strings.Contains(string(raw), "usage: git remote-http")) {
			t.Fatal("measured HTTP helper namespace", string(raw), err)
		}
	}
}
func TestGitRuntimeRejectsMalformedArtifactsAndOuterPin(t *testing.T) {
	for _, kind := range []string{"purpose", "git-pin", "helper-pin", "loader-missing", "path", "sort", "duplicate", "null", "trailing", "truncated", "body", "outer-pin", "outer-size", "manifest-cap", "unknown", "alias", "duplicate-key"} {
		t.Run(kind, func(t *testing.T) {
			m, bodies := gitRuntimeFixture(t)
			switch kind {
			case "purpose":
				m.Purpose = "native-qemu-runtime"
			case "git-pin":
				m.Git.SHA256 = Digest("wrong program")
			case "helper-pin":
				m.HTTP.Bytes++
			case "loader-missing":
				for i, file := range m.Files {
					if file.Path == gitRuntimeLoader {
						m.Files = append(m.Files[:i], m.Files[i+1:]...)
						break
					}
				}
			case "path":
				m.Files[0].Path = "../escape"
			case "sort":
				m.Files[0], m.Files[1] = m.Files[1], m.Files[0]
			case "duplicate":
				m.Files = append(m.Files, m.Files[len(m.Files)-1])
			}
			raw, pins := gitRuntimeFrame(t, m, bodies)
			switch kind {
			case "trailing":
				raw = append(raw, 0)
			case "truncated":
				raw = raw[:len(raw)-1]
			case "body":
				raw[len(raw)-1] ^= 1
			case "outer-pin":
				pins.Artifact.SHA256 = Digest("wrong runtime")
			case "outer-size":
				pins.Artifact.Bytes++
			case "manifest-cap":
				binary.BigEndian.PutUint32(raw[len(gitRuntimeMagic):], gitRuntimeManifestBytes+1)
			case "null", "unknown", "alias", "duplicate-key":
				offset := len(gitRuntimeMagic)
				size := int(binary.BigEndian.Uint32(raw[offset : offset+4]))
				header := raw[offset+4 : offset+4+size]
				switch kind {
				case "null":
					header = bytes.Replace(header, []byte(`"Version":1`), []byte(`"Version":null`), 1)
				case "unknown":
					header = append([]byte(`{"Unknown":1,`), header[1:]...)
				case "alias":
					header = bytes.Replace(header, []byte(`"Version"`), []byte(`"version"`), 1)
				case "duplicate-key":
					header = append([]byte(`{"Version":1,`), header[1:]...)
				}
				var changed bytes.Buffer
				changed.WriteString(gitRuntimeMagic)
				binary.Write(&changed, binary.BigEndian, uint32(len(header)))
				changed.Write(header)
				changed.Write(raw[offset+4+size:])
				raw = changed.Bytes()
			}
			// Re-pin malformed producer bytes so semantic/per-body guards are tested
			// independently of the outer artifact integrity guard.
			if kind != "outer-pin" && kind != "outer-size" {
				pins.Artifact = VMAssetPin{gitRawHash(raw), int64(len(raw))}
			}
			parent := nativeRuntimePrivateParent(t)
			root, err := prepareGitRuntime(context.Background(), parent, bytes.NewReader(raw), pins, nil)
			if root != nil {
				root.Close()
			}
			if err == nil || root != nil {
				t.Fatal("bad artifact accepted")
			}
			if _, err := os.Lstat(filepath.Join(parent, gitRuntimeDirectory)); !os.IsNotExist(err) {
				t.Fatal("failed preparation retained paths", err)
			}
		})
	}
}
func TestGitRuntimeNeverDeletesForeignReplacementOrAcceptsNewPaths(t *testing.T) {
	m, bodies := gitRuntimeFixture(t)
	raw, pins := gitRuntimeFrame(t, m, bodies)
	parent := nativeRuntimePrivateParent(t)
	root, err := prepareGitRuntime(context.Background(), parent, bytes.NewReader(raw), pins, nil)
	if err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(parent, gitRuntimeDirectory)
	if err := os.Rename(original, original+"-held"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(original, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(original, "preserve")
	os.WriteFile(marker, []byte("foreign"), 0600)
	if root.Close() == nil {
		t.Fatal("foreign replacement cleanup succeeded")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "foreign" {
		t.Fatal("foreign data removed")
	}
	// The original immutable tree remains outside the abandoned name; fixture
	// cleanup is authorized only after no process has consumed its descriptor.
	os.Chmod(original+"-held", 0700)
	filepath.WalkDir(original+"-held", func(path string, entry os.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			os.Chmod(path, 0700)
		}
		return nil
	})
	os.RemoveAll(original + "-held")
	parent2 := nativeRuntimePrivateParent(t)
	root, err = prepareGitRuntime(context.Background(), parent2, bytes.NewReader(raw), pins, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	root.root.Chmod(".", 0700)
	file, err := root.root.OpenFile("extra", os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	root.root.Chmod(".", 0555)
	if root.verify() == nil {
		t.Fatal("unmeasured file admitted")
	}
}
func TestGitRuntimeCancellationAndFailedConsumer(t *testing.T) {
	m, bodies := gitRuntimeFixture(t)
	raw, pins := gitRuntimeFrame(t, m, bodies)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := streamGitRuntime(ctx, bytes.NewReader(raw), pins, func(nativeRuntimeFile, io.Reader) error { return nil }); err == nil {
		t.Fatal("cancelled stream accepted")
	}
	if _, err := streamGitRuntime(context.Background(), bytes.NewReader(raw), pins, func(nativeRuntimeFile, io.Reader) error { return nil }); err == nil {
		t.Fatal("unconsumed body accepted")
	}
	parent := nativeRuntimePrivateParent(t)
	active, cancelActive := context.WithCancel(context.Background())
	defer cancelActive()
	if root, err := prepareGitRuntime(active, parent, bytes.NewReader(raw), pins, func(*gitRuntimeRoot) error { cancelActive(); return nil }); err == nil || root != nil {
		if root != nil {
			root.Close()
		}
		t.Fatal("cancelled preparation accepted")
	}
	if _, err := os.Lstat(filepath.Join(parent, gitRuntimeDirectory)); !os.IsNotExist(err) {
		t.Fatal("cancelled preparation retained a root")
	}

}
