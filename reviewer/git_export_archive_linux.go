//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var errGitExportArchive = errors.New("Git export archive requires recovery")

const gitExportWireBytes = int64(len(gitMaterializationMagic) + 8 + gitMaterializationManifestBytes + snapshotPacketBytes + gitMaterializationBlobBytes)
const gitExportArchiveRecords = 128

type gitExportRecord struct {
	Version              int
	Binding              gitMaterializationBinding
	State                string
	Bytes                int64
	WireHash, PacketHash string
}

func (r gitExportRecord) valid() bool {
	if r.Version != 1 || !r.Binding.valid() {
		return false
	}
	switch r.State {
	case "receiving", "discarded":
		return r.Bytes == 0 && r.WireHash == "" && r.PacketHash == ""
	case "staged":
		return r.Bytes > 0 && r.Bytes <= gitExportWireBytes && validAuthHash(r.WireHash) && validAuthHash(r.PacketHash)
	}
	return false
}

// gitExportArchive retains only bounded plain export frames. Its private staging
// methods never confer source qualification or prove worker/broker stop. The
// eventual factory must authenticate the producer and re-open its independent
// stop journal before restoring/publishing any retained source after restart.
type gitExportArchive struct {
	root      *os.Root
	directory string
	identity  os.FileInfo
	lock      *os.File
	operation chan struct{}
	closed    bool
}

func openGitExportArchive(ctx context.Context, directory string) (*gitExportArchive, error) {
	if ctx == nil || ctx.Err() != nil || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || privateDirectory(directory) != nil {
		return nil, errGitExportArchive
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil || resolved != directory {
		return nil, errGitExportArchive
	}
	identity, err := os.Lstat(directory)
	if err != nil {
		return nil, errGitExportArchive
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, errGitExportArchive
	}
	a := &gitExportArchive{root: root, directory: directory, identity: identity, operation: make(chan struct{}, 1)}
	a.operation <- struct{}{}
	good := false
	defer func() {
		if !good {
			if a.lock != nil {
				a.lock.Close()
			}
			root.Close()
		}
	}()
	a.lock, err = openRootNoFollow(root, "git-export.lock", os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil || privateAuthFile(a.lock) != nil {
		return nil, errGitExportArchive
	}
	for {
		if ctx.Err() != nil {
			return nil, errGitExportArchive
		}
		err = unix.Flock(int(a.lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if err != unix.EAGAIN && err != unix.EWOULDBLOCK {
			return nil, errGitExportArchive
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, errGitExportArchive
		case <-timer.C:
		}
	}
	if _, err = a.records(); err != nil {
		return nil, errGitExportArchive
	}
	good = true
	return a, nil
}
func (a *gitExportArchive) enter(ctx context.Context) bool {
	if a == nil || ctx == nil {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case <-a.operation:
		if ctx.Err() != nil {
			a.operation <- struct{}{}
			return false
		}
		return true
	}
}
func (a *gitExportArchive) leave() { a.operation <- struct{}{} }
func (a *gitExportArchive) verifyRoot() error {
	if a.closed || a.root == nil || a.lock == nil {
		return errGitExportArchive
	}
	path, err := os.Lstat(a.directory)
	if err != nil || path.Mode()&os.ModeSymlink != 0 || !os.SameFile(path, a.identity) || privateDirectory(a.directory) != nil {
		return errGitExportArchive
	}
	anchored, err := a.root.Stat(".")
	if err != nil || !os.SameFile(anchored, a.identity) {
		return errGitExportArchive
	}
	held, err := a.lock.Stat()
	atPath, pathErr := a.root.Lstat("git-export.lock")
	if err != nil || pathErr != nil || privateAuthFile(a.lock) != nil || !os.SameFile(held, atPath) {
		return errGitExportArchive
	}
	return nil
}
func gitExportDirectory(attempt string) string { return "export-" + attempt }
func (a *gitExportArchive) attempt(attempt string) (*os.Root, error) {
	if !gitQuarantineHex(attempt, 32) || a.verifyRoot() != nil {
		return nil, errGitExportArchive
	}
	name := gitExportDirectory(attempt)
	info, err := a.root.Lstat(name)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || info.Mode()&os.ModeSymlink != 0 || privateDirectory(filepath.Join(a.directory, name)) != nil {
		return nil, errGitExportArchive
	}
	root, err := a.root.OpenRoot(name)
	if err != nil {
		return nil, errGitExportArchive
	}
	current, err := root.Stat(".")
	if err != nil || !os.SameFile(current, info) {
		root.Close()
		return nil, errGitExportArchive
	}
	return root, nil
}
func (a *gitExportArchive) verifyAttempt(root *os.Root, attempt string) error {
	if a.verifyRoot() != nil {
		return errGitExportArchive
	}
	anchored, err := root.Stat(".")
	current, pathErr := a.root.Lstat(gitExportDirectory(attempt))
	if err != nil || pathErr != nil || !current.IsDir() || current.Mode().Perm() != 0700 || !os.SameFile(anchored, current) {
		return errGitExportArchive
	}
	return nil
}
func readGitExportRecord(root *os.Root) (gitExportRecord, error) {
	var r gitExportRecord
	raw, err := readAuth(root, "archive.json", false)
	if err != nil || len(raw) > 8192 || json.Unmarshal(raw, &r) != nil {
		return r, errGitExportArchive
	}
	canonical, err := json.Marshal(r)
	if err != nil || !bytes.Equal(raw, canonical) || !r.valid() {
		return gitExportRecord{}, errGitExportArchive
	}
	directory, err := root.Open(".")
	if err != nil {
		return r, errGitExportArchive
	}
	defer directory.Close()
	entries, err := directory.ReadDir(4)
	if err != nil && err != io.EOF || len(entries) > 2 {
		return r, errGitExportArchive
	}
	payload := false
	for _, entry := range entries {
		if entry.Name() != "archive.json" && entry.Name() != "payload.bin" {
			return r, errGitExportArchive
		}
		if !entry.Type().IsRegular() {
			return r, errGitExportArchive
		}
		if entry.Name() == "payload.bin" {
			payload = true
		}
	}
	if r.State == "staged" && (!payload || len(entries) != 2) {
		return r, errGitExportArchive
	}
	return r, nil
}
func writeGitExportRecord(root *os.Root, r gitExportRecord) error {
	if !r.valid() {
		return errGitExportArchive
	}
	raw, err := json.Marshal(r)
	if err != nil || len(raw) > 8192 || writeAuth(root, "archive.json", raw) != nil {
		return errGitExportArchive
	}
	return nil
}
func (a *gitExportArchive) records() ([]gitExportRecord, error) {
	if a.verifyRoot() != nil {
		return nil, errGitExportArchive
	}
	dir, err := a.root.Open(".")
	if err != nil {
		return nil, errGitExportArchive
	}
	defer dir.Close()
	entries, err := dir.ReadDir(gitExportArchiveRecords*4 + 2)
	if err != nil && err != io.EOF || len(entries) > gitExportArchiveRecords*4+1 {
		return nil, errGitExportArchive
	}
	retirements, err := gitRetirementIntents(a)
	if err != nil {
		return nil, errGitExportArchive
	}
	intents := map[string]gitRetirement{}
	for _, intent := range retirements {
		intents[intent.Unit.Binding.Attempt] = intent
	}
	out := []gitExportRecord{}
	for _, entry := range entries {
		if entry.Name() == "git-export.lock" {
			continue
		}
		if strings.HasPrefix(entry.Name(), "retire-") {
			attempt := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "retire-"), ".json")
			if _, ok := intents[attempt]; !ok || entry.Name() != gitRetirementName(attempt) {
				return nil, errGitExportArchive
			}
			continue
		}
		if strings.HasPrefix(entry.Name(), "retired-") {
			matched := false
			for attempt, intent := range intents {
				for kind, id := range map[string]gitKernelFileID{"unit": intent.UnitDirectory, "export": intent.ExportDirectory} {
					if entry.Name() == gitRetirementTrash(kind, attempt) && id.Inode != 0 {
						present, err := gitRetirementPathIdentity(a.root, entry.Name(), id)
						if err != nil || !present {
							return nil, errGitExportArchive
						}
						matched = true
					}
				}
			}
			if !matched {
				return nil, errGitExportArchive
			}
			continue
		}
		attempt := strings.TrimPrefix(entry.Name(), "export-")
		if !strings.HasPrefix(entry.Name(), "export-") || !gitQuarantineHex(attempt, 32) || !entry.IsDir() {
			return nil, errGitExportArchive
		}
		root, err := a.attempt(attempt)
		if err != nil {
			return nil, errGitExportArchive
		}
		r, err := readGitExportRecord(root)
		root.Close()
		if err != nil || r.Binding.Attempt != attempt {
			return nil, errGitExportArchive
		}
		out = append(out, r)
		if len(out) > gitExportArchiveRecords {
			return nil, errGitExportArchive
		}
	}
	return out, nil
}

type gitExportWriter struct {
	ctx    context.Context
	output io.Writer
	digest hash.Hash
	bytes  int64
}

func (w *gitExportWriter) Write(data []byte) (int, error) {
	if w.ctx.Err() != nil || int64(len(data)) > gitExportWireBytes-w.bytes {
		return 0, errGitExportArchive
	}
	n, err := w.output.Write(data)
	if n > 0 {
		w.bytes += int64(n)
		w.digest.Write(data[:n])
	}
	if err != nil {
		return n, errGitExportArchive
	}
	if n != len(data) {
		return n, io.ErrShortWrite
	}
	return n, nil
}

type gitExportIntake struct {
	io.Reader
	io.Closer
}

// stage spends the attempt directory before intake. Partial/failed intake stays
// receiving and blocks all new staging; no retry or refetch follows a crash.
// Its return is private staging only, even after durable persistence succeeds.
// An exact refusal record also stays receiving and returns the bare refusal.
func (a *gitExportArchive) stage(ctx context.Context, binding gitMaterializationBinding, input io.ReadCloser) (*gitMaterialization, error) {
	if input == nil {
		return nil, errGitExportArchive
	}
	defer input.Close()
	if !a.enter(ctx) {
		return nil, errGitExportArchive
	}
	defer a.leave()
	if a.verifyRoot() != nil || !binding.valid() {
		return nil, errGitExportArchive
	}
	records, err := a.records()
	if err != nil || len(records) >= gitExportArchiveRecords {
		return nil, errGitExportArchive
	}
	for _, record := range records {
		if record.State != "staged" && record.State != "discarded" || record.Binding.Attempt == binding.Attempt {
			return nil, errGitExportArchive
		}
	}
	if a.root.Mkdir(gitExportDirectory(binding.Attempt), 0700) != nil || syncAuthDirectory(a.root) != nil {
		return nil, errGitExportArchive
	}
	root, err := a.attempt(binding.Attempt)
	if err != nil {
		return nil, errGitExportArchive
	}
	defer root.Close()
	record := gitExportRecord{Version: 1, Binding: binding, State: "receiving"}
	if writeGitExportRecord(root, record) != nil {
		return nil, errGitExportArchive
	}
	file, err := openRootNoFollow(root, "payload.bin", os.O_CREATE|os.O_EXCL|os.O_WRONLY|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, errGitExportArchive
	}
	defer file.Close()
	writer := &gitExportWriter{ctx: ctx, output: file, digest: sha256.New()}
	intake := &gitExportIntake{Reader: io.TeeReader(io.LimitReader(input, gitExportWireBytes+1), writer), Closer: input}
	materialized, err := receiveGitMaterialization(ctx, intake, binding)
	if refusal, ok := bareCaptureRefusal(ctx, err); ok {
		// The attempt stays receiving like any failed intake: the factory
		// discards it after proved stop and it can never reopen as source.
		return nil, refusal
	}
	if err != nil {
		return nil, errGitExportArchive
	}
	good := false
	defer func() {
		if !good {
			materialized.Close()
		}
	}()
	if ctx.Err() != nil || file.Chmod(0400) != nil || file.Sync() != nil {
		return nil, errGitExportArchive
	}
	payloadInfo, err := file.Stat()
	atPath, pathErr := root.Lstat("payload.bin")
	if err != nil || pathErr != nil || privateAuthFile(file) != nil || payloadInfo.Mode().Perm() != 0400 || payloadInfo.Size() != writer.bytes || !os.SameFile(payloadInfo, atPath) || a.verifyAttempt(root, binding.Attempt) != nil || file.Close() != nil || syncAuthDirectory(root) != nil {
		return nil, errGitExportArchive
	}
	record.State = "staged"
	record.Bytes = writer.bytes
	record.WireHash = hex.EncodeToString(writer.digest.Sum(nil))
	record.PacketHash = Digest(materialized.packet)
	if writeGitExportRecord(root, record) != nil || a.verifyAttempt(root, binding.Attempt) != nil || ctx.Err() != nil {
		return nil, errGitExportArchive
	}
	good = true
	return materialized, nil
}

type gitExportHashWriter struct {
	digest hash.Hash
	bytes  int64
}

func (w *gitExportHashWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > gitExportWireBytes-w.bytes {
		return 0, errGitExportArchive
	}
	w.bytes += int64(len(data))
	return w.digest.Write(data)
}

// reopen performs one bounded framing/hash validation pass into fresh sealed
// blobs, never a host Git/object parser or a replay of acquisition. Independent
// authenticated stop/eligibility evidence is still required by the real factory.
func (a *gitExportArchive) reopen(ctx context.Context, binding gitMaterializationBinding) (*gitMaterialization, error) {
	if !a.enter(ctx) {
		return nil, errGitExportArchive
	}
	defer a.leave()
	if !binding.valid() {
		return nil, errGitExportArchive
	}
	root, err := a.attempt(binding.Attempt)
	if err != nil {
		return nil, errGitExportArchive
	}
	defer root.Close()
	record, err := readGitExportRecord(root)
	if err != nil || record.State != "staged" || record.Binding != binding {
		return nil, errGitExportArchive
	}
	file, err := openRootNoFollow(root, "payload.bin", os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, errGitExportArchive
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || privateAuthFile(file) != nil || before.Mode().Perm() != 0400 || before.Size() != record.Bytes {
		return nil, errGitExportArchive
	}
	atPath, err := root.Lstat("payload.bin")
	if err != nil || !os.SameFile(before, atPath) {
		return nil, errGitExportArchive
	}
	duplicate, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, errGitExportArchive
	}
	intakeFile := os.NewFile(uintptr(duplicate), "git-export-payload")
	defer intakeFile.Close()
	digest := &gitExportHashWriter{digest: sha256.New()}
	materialized, err := receiveGitMaterialization(ctx, &gitExportIntake{Reader: io.TeeReader(io.LimitReader(intakeFile, gitExportWireBytes+1), digest), Closer: intakeFile}, binding)
	if err != nil {
		return nil, errGitExportArchive
	}
	good := false
	defer func() {
		if !good {
			materialized.Close()
		}
	}()
	after, err := file.Stat()
	atPath, pathErr := root.Lstat("payload.bin")
	if err != nil || pathErr != nil || !os.SameFile(before, after) || !os.SameFile(before, atPath) || before.Size() != after.Size() || privateAuthFile(file) != nil || digest.bytes != record.Bytes || hex.EncodeToString(digest.digest.Sum(nil)) != record.WireHash || Digest(materialized.packet) != record.PacketHash || a.verifyAttempt(root, binding.Attempt) != nil || ctx.Err() != nil {
		return nil, errGitExportArchive
	}
	good = true
	return materialized, nil
}
func (a *gitExportArchive) Close(ctx context.Context) error {
	if !a.enter(ctx) {
		return errGitExportArchive
	}
	defer a.leave()
	if a.closed {
		return nil
	}
	// No intake/reader is active while this operation token is held. A timeout
	// acquiring it retains descriptors/flock for an explicit close retry.
	lockErr := a.lock.Close()
	rootErr := a.root.Close()
	a.closed = true
	if lockErr != nil || rootErr != nil {
		return errGitExportArchive
	}
	return nil
}
