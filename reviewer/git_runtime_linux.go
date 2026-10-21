//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

var errGitRuntime = errors.New("measured Git runtime refused")

const gitRuntimeMagic = "SFL-GIT-RUNTIME-1\n"
const gitRuntimeManifestBytes = 64 << 10
const gitRuntimeDataBytes = 128 << 20
const gitRuntimeDirectory = "git-runtime"
const gitRuntimeProgram = "usr/bin/git"
const gitRuntimeHTTP = "usr/lib/git-core/git-remote-http"
const gitRuntimeLoader = "lib64/ld-linux-x86-64.so.2"

// GitRuntimePins are release inputs. Artifact binds all metadata/library bytes;
// per-program pins additionally bind the two specific permitted executables.
// Producing a pin is not release promotion, runtime qualification or activation.
type GitRuntimePins struct{ Git, HTTP, Artifact VMAssetPin }
type GitRuntimeBuild struct {
	Pins     GitRuntimePins
	Manifest json.RawMessage
	Artifact []byte
}
type gitRuntimeManifest struct {
	Version   int
	Purpose   string
	Git, HTTP VMAssetPin
	Files     []nativeRuntimeFile
}

func gitRuntimePath(path string) bool {
	if path == gitRuntimeProgram || path == gitRuntimeHTTP || path == gitRuntimeLoader {
		return true
	}
	for _, prefix := range []string{"lib/x86_64-linux-gnu/", "usr/lib/x86_64-linux-gnu/"} {
		if strings.HasPrefix(path, prefix) {
			return nativeRuntimeLibrary.MatchString(strings.TrimPrefix(path, prefix))
		}
	}
	return false
}
func gitRuntimePin(pin VMAssetPin, max int64) bool {
	return validAuthHash(pin.SHA256) && pin.Bytes > 0 && pin.Bytes <= max
}
func (m gitRuntimeManifest) valid(pins GitRuntimePins) bool {
	if m.Version != 1 || m.Purpose != "isolated-git-acquisition" || m.Git != pins.Git || m.HTTP != pins.HTTP || !gitRuntimePin(pins.Git, 64<<20) || !gitRuntimePin(pins.HTTP, 64<<20) || !gitRuntimePin(pins.Artifact, gitRuntimeDataBytes+gitRuntimeManifestBytes+int64(len(gitRuntimeMagic))+4) || len(m.Files) < 4 || len(m.Files) > 128 {
		return false
	}
	previous := ""
	var total int64
	seen := map[string]bool{}
	for _, file := range m.Files {
		if !gitRuntimePath(file.Path) || file.Path <= previous || !validAuthHash(file.SHA256) || file.Bytes <= 0 || file.Bytes > 64<<20 || file.Bytes > gitRuntimeDataBytes-total {
			return false
		}
		if file.Path == gitRuntimeProgram && (file.SHA256 != pins.Git.SHA256 || file.Bytes != pins.Git.Bytes) || file.Path == gitRuntimeHTTP && (file.SHA256 != pins.HTTP.SHA256 || file.Bytes != pins.HTTP.Bytes) {
			return false
		}
		previous = file.Path
		total += file.Bytes
		seen[file.Path] = true
	}
	return seen[gitRuntimeProgram] && seen[gitRuntimeHTTP] && seen[gitRuntimeLoader] && (seen["lib/x86_64-linux-gnu/libc.so.6"] || seen["usr/lib/x86_64-linux-gnu/libc.so.6"])
}

// BuildGitRuntime is offline release tooling. It reads explicitly pinned,
// root-owned installed programs and their root-owned ELF dependencies. It does
// not exec them, invoke ldd, run a shell, start a unit or inspect credentials.
// Only x86-64 ELF with the fixed loader/default library paths is accepted.
func BuildGitRuntime(ctx context.Context, git, http VMBootAsset) (GitRuntimeBuild, error) {
	zero := GitRuntimeBuild{}
	if ctx == nil || ctx.Err() != nil {
		return zero, errGitRuntime
	}
	bodies := map[string][]byte{}
	read := func(path string, pin *VMAssetPin) ([]byte, error) {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return nil, errGitRuntime
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, errGitRuntime
		}
		file, err := os.OpenFile(resolved, os.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return nil, errGitRuntime
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 64<<20 || info.Mode().Perm()&0022 != 0 {
			return nil, errGitRuntime
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || stat.Nlink != 1 {
			return nil, errGitRuntime
		}
		data, err := io.ReadAll(io.LimitReader(file, (64<<20)+1))
		if err != nil || int64(len(data)) != info.Size() || ctx.Err() != nil || pin != nil && (int64(len(data)) != pin.Bytes || gitRawHash(data) != pin.SHA256) {
			return nil, errGitRuntime
		}
		return data, nil
	}
	gitPin, httpPin := VMAssetPin{git.SHA256, git.Bytes}, VMAssetPin{http.SHA256, http.Bytes}
	if !gitRuntimePin(gitPin, 64<<20) || !gitRuntimePin(httpPin, 64<<20) {
		return zero, errGitRuntime
	}
	for _, entry := range []struct {
		path, name string
		pin        VMAssetPin
	}{{git.Path, gitRuntimeProgram, gitPin}, {http.Path, gitRuntimeHTTP, httpPin}} {
		data, err := read(entry.path, &entry.pin)
		if err != nil {
			return zero, err
		}
		bodies[entry.name] = data
	}
	queue := []string{gitRuntimeProgram, gitRuntimeHTTP}
	checked := map[string]bool{}
	total := int64(len(bodies[gitRuntimeProgram]) + len(bodies[gitRuntimeHTTP]))
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if checked[name] {
			continue
		}
		checked[name] = true
		if ctx.Err() != nil {
			return zero, errGitRuntime
		}
		file, err := elf.NewFile(bytes.NewReader(bodies[name]))
		if err != nil || file.Class != elf.ELFCLASS64 || file.Data != elf.ELFDATA2LSB || file.Machine != elf.EM_X86_64 {
			if file != nil {
				file.Close()
			}
			return zero, errGitRuntime
		}
		for _, tag := range []elf.DynTag{elf.DT_RPATH, elf.DT_RUNPATH, elf.DT_AUDIT, elf.DT_DEPAUDIT, elf.DT_FILTER, elf.DT_AUXILIARY} {
			values, err := file.DynValue(tag)
			if err != nil || len(values) != 0 {
				file.Close()
				return zero, errGitRuntime
			}
		}
		dependencies := []string{}
		for _, program := range file.Progs {
			if program.Type == elf.PT_INTERP {
				if program.Filesz == 0 || program.Filesz > 256 {
					file.Close()
					return zero, errGitRuntime
				}
				raw, err := io.ReadAll(io.LimitReader(program.Open(), 257))
				if err != nil || string(raw) != "/"+gitRuntimeLoader+"\x00" {
					file.Close()
					return zero, errGitRuntime
				}
				dependencies = append(dependencies, "/"+gitRuntimeLoader)
			}
		}
		needed, err := file.DynString(elf.DT_NEEDED)
		file.Close()
		if err != nil {
			return zero, errGitRuntime
		}
		for _, soname := range needed {
			if soname == "ld-linux-x86-64.so.2" {
				dependencies = append(dependencies, "/"+gitRuntimeLoader)
				continue
			}
			if !nativeRuntimeLibrary.MatchString(soname) {
				return zero, errGitRuntime
			}
			chosen := ""
			for _, prefix := range []string{"/lib/x86_64-linux-gnu/", "/usr/lib/x86_64-linux-gnu/"} {
				if _, err := os.Stat(prefix + soname); err == nil {
					chosen = prefix + soname
					break
				}
			}
			if chosen == "" {
				return zero, errGitRuntime
			}
			dependencies = append(dependencies, chosen)
		}
		for _, source := range dependencies {
			target := strings.TrimPrefix(source, "/")
			if !gitRuntimePath(target) {
				return zero, errGitRuntime
			}
			if _, exists := bodies[target]; exists {
				continue
			}
			data, err := read(source, nil)
			if err != nil {
				return zero, err
			}
			total += int64(len(data))
			if len(bodies) >= 128 || total > gitRuntimeDataBytes {
				return zero, errGitRuntime
			}
			bodies[target] = data
			queue = append(queue, target)
		}
	}
	paths := make([]string, 0, len(bodies))
	for path := range bodies {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	m := gitRuntimeManifest{Version: 1, Purpose: "isolated-git-acquisition", Git: gitPin, HTTP: httpPin, Files: []nativeRuntimeFile{}}
	for _, path := range paths {
		data := bodies[path]
		m.Files = append(m.Files, nativeRuntimeFile{path, gitRawHash(data), int64(len(data))})
	}
	raw, err := json.Marshal(m)
	if err != nil || len(raw) > gitRuntimeManifestBytes {
		return zero, errGitRuntime
	}
	var output bytes.Buffer
	output.WriteString(gitRuntimeMagic)
	binary.Write(&output, binary.BigEndian, uint32(len(raw)))
	output.Write(raw)
	for _, path := range paths {
		output.Write(bodies[path])
	}
	pins := GitRuntimePins{gitPin, httpPin, VMAssetPin{gitRawHash(output.Bytes()), int64(output.Len())}}
	if !m.valid(pins) {
		return zero, errGitRuntime
	}
	return GitRuntimeBuild{pins, raw, output.Bytes()}, nil
}

func streamGitRuntime(ctx context.Context, input io.Reader, pins GitRuntimePins, consume func(nativeRuntimeFile, io.Reader) error) (gitRuntimeManifest, error) {
	zero := gitRuntimeManifest{}
	if ctx == nil || ctx.Err() != nil || input == nil || consume == nil || !gitRuntimePin(pins.Artifact, gitRuntimeDataBytes+gitRuntimeManifestBytes+int64(len(gitRuntimeMagic))+4) {
		return zero, errGitRuntime
	}
	hash := sha256.New()
	limited := &io.LimitedReader{R: input, N: pins.Artifact.Bytes + 1}
	reader := io.TeeReader(limited, hash)
	magic := make([]byte, len(gitRuntimeMagic))
	if _, err := io.ReadFull(reader, magic); err != nil || string(magic) != gitRuntimeMagic {
		return zero, errGitRuntime
	}
	var size uint32
	if binary.Read(reader, binary.BigEndian, &size) != nil || size == 0 || size > gitRuntimeManifestBytes {
		return zero, errGitRuntime
	}
	raw := make([]byte, size)
	if _, err := io.ReadFull(reader, raw); err != nil {
		return zero, errGitRuntime
	}
	var m gitRuntimeManifest
	if json.Unmarshal(raw, &m) != nil || !m.valid(pins) {
		return zero, errGitRuntime
	}
	canonical, err := json.Marshal(m)
	if err != nil || !bytes.Equal(canonical, raw) {
		return zero, errGitRuntime
	}
	for _, file := range m.Files {
		if ctx.Err() != nil {
			return zero, errGitRuntime
		}
		body := &io.LimitedReader{R: reader, N: file.Bytes}
		content := sha256.New()
		if consume(file, io.TeeReader(body, content)) != nil || body.N != 0 || hex.EncodeToString(content.Sum(nil)) != file.SHA256 {
			return zero, errGitRuntime
		}
	}
	var trailing [1]byte
	n, err := io.ReadFull(reader, trailing[:])
	if n != 0 || err != io.EOF || limited.N != 1 || hex.EncodeToString(hash.Sum(nil)) != pins.Artifact.SHA256 || ctx.Err() != nil {
		return zero, errGitRuntime
	}
	return m, nil
}

// A Git runtime reuses only the anchored immutable filesystem ownership code.
// Its artifact, allowed paths, executable identities and directory are distinct
// from QEMU. The factory owns its lifetime until the entire Git unit is stopped.
type gitRuntimeRoot struct {
	*nativeRuntimeRoot
	manifest gitRuntimeManifest
}

func prepareGitRuntime(ctx context.Context, parentPath string, input io.Reader, pins GitRuntimePins, created func(*gitRuntimeRoot) error) (_ *gitRuntimeRoot, result error) {
	if ctx == nil || ctx.Err() != nil || !filepath.IsAbs(parentPath) || filepath.Clean(parentPath) != parentPath || privateDirectory(parentPath) != nil {
		return nil, errGitRuntime
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return nil, errGitRuntime
	}
	if parent.Mkdir(gitRuntimeDirectory, 0700) != nil {
		parent.Close()
		return nil, errGitRuntime
	}
	identity, err := parent.Lstat(gitRuntimeDirectory)
	if err != nil {
		parent.Close()
		return nil, errGitRuntime
	}
	root, err := parent.OpenRoot(gitRuntimeDirectory)
	if err != nil {
		parent.Close()
		return nil, errGitRuntime
	}
	prepared := &gitRuntimeRoot{nativeRuntimeRoot: &nativeRuntimeRoot{parent: parent, root: root, directory: gitRuntimeDirectory, identity: identity, directories: map[string]bool{".": true}}}
	defer func() {
		if result != nil {
			result = errors.Join(result, prepared.Close())
		}
	}()
	if created != nil && created(prepared) != nil {
		return nil, errGitRuntime
	}
	for _, path := range []string{"proc", "dev", "work", "broker"} {
		prepared.directories[path] = true
		if root.Mkdir(path, 0700) != nil {
			return nil, errGitRuntime
		}
	}
	m, err := streamGitRuntime(ctx, input, pins, func(file nativeRuntimeFile, body io.Reader) error {
		parts := strings.Split(filepath.Dir(file.Path), "/")
		for index := range parts {
			prepared.directories[strings.Join(parts[:index+1], "/")] = true
		}
		if root.MkdirAll(filepath.Dir(file.Path), 0700) != nil {
			return errGitRuntime
		}
		output, err := root.OpenFile(file.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|unix.O_NOFOLLOW, 0600)
		if err != nil {
			return errGitRuntime
		}
		n, copyErr := io.Copy(output, body)
		syncErr := output.Sync()
		closeErr := output.Close()
		if copyErr != nil || syncErr != nil || closeErr != nil || n != file.Bytes {
			return errGitRuntime
		}
		mode := os.FileMode(0444)
		if file.Path == gitRuntimeProgram || file.Path == gitRuntimeHTTP || file.Path == gitRuntimeLoader {
			mode = 0555
		}
		return root.Chmod(file.Path, mode)
	})
	if err != nil {
		return nil, err
	}
	prepared.manifest = m
	prepared.files = append([]nativeRuntimeFile(nil), m.Files...)
	for path := range prepared.directories {
		if root.Chmod(path, 0555) != nil {
			return nil, errGitRuntime
		}
	}
	prepared.ready = true
	if prepared.verify() != nil {
		return nil, errGitRuntime
	}
	return prepared, nil
}
