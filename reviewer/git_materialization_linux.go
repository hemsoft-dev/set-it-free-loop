//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

const gitMaterializationMagic = "SFL-GIT-MATERIALIZATION-1\n"
const gitMaterializationManifestBytes = 256 << 10
const gitMaterializationBlobBytes = 32 << 20
const gitMaterializationEntries = 3 * snapshotFileLimit

var errGitMaterialization = errors.New("Git materialization refused")

// A worker that reaches a permanent capture refusal exports this record instead
// of a materialization. Its marker has the materialization marker's length, so
// the receiver reads one fixed prefix before choosing either frame.
const gitCaptureRefusalMagic = "SFL-GIT-CAPTURE-REFUSAL-1\n"
const gitCaptureRefusalBytes = 4 << 10

// gitCaptureRefusalRecord binds a refusal reason to the exact attempt. It has
// no manifest, packet or blob, so a refusal can never carry source data.
type gitCaptureRefusalRecord struct {
	Version int
	Binding gitMaterializationBinding
	Reason  string
}

// errGitMaterializationByteLimit refuses a sealed blob larger than the caller's
// remaining byte budget. It is still a materialization refusal.
var errGitMaterializationByteLimit = fmt.Errorf("%w: %w", errGitMaterialization, ErrSourceByteLimit)

// These private bindings are supplied by the trusted acquisition factory, not
// by a candidate or provider. Matching metadata alone does not prove unit stop.
type gitMaterializationBinding struct {
	Attempt, ReleaseHash, RuntimeHash string
	Snapshot                          Snapshot
}

func (b gitMaterializationBinding) valid() bool {
	return gitQuarantineHex(b.Attempt, 32) && validAuthHash(b.ReleaseHash) && validAuthHash(b.RuntimeHash) && b.Snapshot.Validate() == nil && len(b.Snapshot.Head) == len(b.Snapshot.Base) && len(b.Snapshot.Head) == len(b.Snapshot.MergeBase)
}

type gitMaterializationEntry struct {
	Revision, Path, Mode, Object, State string
}
type gitMaterializationBlob struct {
	Object, SHA256 string
	Bytes          int
}
type gitMaterializationManifest struct {
	Version    int
	Binding    gitMaterializationBinding
	Inventory  GitAcquisitionInventory
	PacketHash string
	Entries    []gitMaterializationEntry
	Blobs      []gitMaterializationBlob
}

func gitRawHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func gitBlobHash(data []byte, length int) string {
	var h hash.Hash
	if length == 40 {
		h = sha1.New()
	} else {
		h = sha256.New()
	}
	fmt.Fprintf(h, "blob %d%c", len(data), 0)
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}
func gitMaterializationRevisions(s Snapshot) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, revision := range []string{s.Head, s.Base, s.MergeBase} {
		if !seen[revision] {
			seen[revision] = true
			out = append(out, revision)
		}
	}
	return out
}
func gitMaterializationPacket(packet []byte, s Snapshot) (snapshotPacket, error) {
	var p snapshotPacket
	if len(packet) == 0 || len(packet) > snapshotPacketBytes || json.Unmarshal(packet, &p) != nil {
		return p, errGitMaterialization
	}
	canonical, err := json.Marshal(p)
	if err != nil || !bytes.Equal(canonical, packet) || p.Version != "sfl-snapshot-evidence-1" || p.Snapshot != s || p.Files == nil || p.Coverage == nil || len(p.Files) > snapshotFileLimit {
		return p, errGitMaterialization
	}
	var raw bytes.Buffer
	previous := ""
	for _, f := range p.Files {
		if !validPath(f.Path) || f.Path <= previous {
			return p, errGitMaterialization
		}
		previous = f.Path
		fmt.Fprintf(&raw, ":%s %s %s %s %s%c%s%c", f.OldMode, f.NewMode, f.OldObject, f.NewObject, f.Status, 0, f.Path, 0)
	}
	if _, err := parseSnapshotChanges(raw.Bytes(), len(s.Head)); err != nil {
		return p, errGitMaterialization
	}
	return p, nil
}

// writeGitMaterialization runs ONLY inside the protected acquisition worker.
// Verification, diff capture, tree lookup and decompression all parse untrusted
// Git objects and require the same aggregate unit limits as fetch. Tests call
// it directly only for small trusted synthetic repositories.
func writeGitMaterialization(ctx context.Context, output io.Writer, store GitStore, b gitMaterializationBinding, bounds GitAcquisitionBounds) error {
	if ctx == nil || ctx.Err() != nil || output == nil || !b.valid() {
		return errGitMaterialization
	}
	inventory, err := verifyGitQuarantine(ctx, store, b.Snapshot, bounds)
	if refusal, ok := bareCaptureRefusal(ctx, err); ok {
		return writeGitCaptureRefusal(ctx, output, b, refusal.Reason)
	}
	if err != nil {
		return errGitMaterialization
	}
	evidence, err := CaptureSnapshotEvidence(ctx, store, b.Snapshot)
	if refusal, ok := bareCaptureRefusal(ctx, err); ok {
		return writeGitCaptureRefusal(ctx, output, b, refusal.Reason)
	}
	if err != nil {
		return errGitMaterialization
	}
	packet := []byte(evidence.Packet())
	p, err := gitMaterializationPacket(packet, b.Snapshot)
	if err != nil {
		return err
	}
	m := gitMaterializationManifest{Version: 1, Binding: b, Inventory: inventory, PacketHash: gitRawHash(packet), Entries: []gitMaterializationEntry{}, Blobs: []gitMaterializationBlob{}}
	blobs := map[string][]byte{}
	used := 0
	load := func(id string) error {
		if strings.Trim(id, "0") == "" {
			return nil
		}
		if _, ok := blobs[id]; ok {
			return nil
		}
		raw, err := store.command(ctx, 32, "cat-file", "-s", id)
		size, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil || parseErr != nil || size < 0 || size > snapshotObjectBytes || size > gitMaterializationBlobBytes-used {
			return errGitMaterialization
		}
		data, err := store.command(ctx, max(1, size), "cat-file", "blob", id)
		if err != nil || len(data) != size || gitBlobHash(data, len(id)) != id {
			return errGitMaterialization
		}
		blobs[id] = data
		used += size
		return nil
	}
	// Reserve the blobs needed to reproduce admitted inline patches first.
	// Additional reader context may be omitted, but an included patch may not.
	for _, file := range p.Files {
		if file.Patch != "" {
			if load(file.OldObject) != nil || load(file.NewObject) != nil {
				return errGitMaterialization
			}
		}
	}
	for _, revision := range gitMaterializationRevisions(b.Snapshot) {
		for _, file := range p.Files {
			if ctx.Err() != nil {
				return errGitMaterialization
			}
			entry := gitMaterializationEntry{Revision: revision, Path: file.Path, Mode: "000000", State: "absent"}
			raw, err := store.command(ctx, 2048, "ls-tree", "-z", revision, "--", ":(literal)"+file.Path)
			// ls-tree uses literal pathspecs; no candidate selects flags or a revision.
			if err != nil {
				return errGitMaterialization
			}
			if len(raw) != 0 {
				parts := bytes.SplitN(raw, []byte{'\t'}, 2)
				if len(parts) != 2 || string(parts[1]) != file.Path+"\x00" {
					return errGitMaterialization
				}
				fields := strings.Fields(string(parts[0]))
				if len(fields) != 3 || !objectID.MatchString(fields[2]) || len(fields[2]) != len(b.Snapshot.Head) {
					return errGitMaterialization
				}
				entry.Mode, entry.Object = fields[0], fields[2]
				switch entry.Mode {
				case "100644", "100755":
					if fields[1] != "blob" {
						return errGitMaterialization
					}
					entry.State = "blob"
					if load(entry.Object) != nil {
						entry.State = "omitted"
					}
				case "120000":
					if fields[1] != "blob" {
						return errGitMaterialization
					}
					entry.State = "nonfile"
				case "160000":
					if fields[1] != "commit" {
						return errGitMaterialization
					}
					entry.State = "nonfile"
				case "040000":
					if fields[1] != "tree" {
						return errGitMaterialization
					}
					entry.State = "nonfile"
				default:
					return errGitMaterialization
				}
			}
			m.Entries = append(m.Entries, entry)
		}
	}
	ids := make([]string, 0, len(blobs))
	for id := range blobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		data := blobs[id]
		m.Blobs = append(m.Blobs, gitMaterializationBlob{id, gitRawHash(data), len(data)})
	}
	if _, err := validateGitMaterialization(m, packet, b); err != nil {
		return err
	}
	manifest, err := json.Marshal(m)
	if err != nil || len(manifest) > gitMaterializationManifestBytes {
		return errGitMaterialization
	}
	write := func(raw []byte) error {
		if ctx.Err() != nil {
			return errGitMaterialization
		}
		n, err := output.Write(raw)
		if err != nil || n != len(raw) {
			return errGitMaterialization
		}
		return nil
	}
	frame := func(raw []byte) error {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(raw)))
		if write(size[:]) != nil || write(raw) != nil {
			return errGitMaterialization
		}
		return nil
	}
	if write([]byte(gitMaterializationMagic)) != nil || frame(manifest) != nil || frame(packet) != nil {
		return errGitMaterialization
	}
	for _, id := range ids {
		if write(blobs[id]) != nil {
			return errGitMaterialization
		}
	}
	if ctx.Err() != nil {
		return errGitMaterialization
	}
	return nil
}

// writeGitCaptureRefusal runs ONLY inside the protected acquisition worker,
// before any materialization byte is written. The record is the whole export.
func writeGitCaptureRefusal(ctx context.Context, output io.Writer, b gitMaterializationBinding, reason string) error {
	if ctx == nil || ctx.Err() != nil || output == nil || !b.valid() || !(CaptureRefusal{reason}).known() {
		return errGitMaterialization
	}
	raw, err := json.Marshal(gitCaptureRefusalRecord{1, b, reason})
	if err != nil || len(raw) > gitCaptureRefusalBytes {
		return errGitMaterialization
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(raw)))
	for _, part := range [][]byte{[]byte(gitCaptureRefusalMagic), size[:], raw} {
		if ctx.Err() != nil {
			return errGitMaterialization
		}
		n, err := output.Write(part)
		if err != nil || n != len(part) {
			return errGitMaterialization
		}
	}
	return nil
}

func validateGitMaterialization(m gitMaterializationManifest, packet []byte, expected gitMaterializationBinding) (snapshotPacket, error) {
	zero := snapshotPacket{}
	if !expected.valid() || m.Version != 1 || m.Binding != expected || m.PacketHash != gitRawHash(packet) || m.Entries == nil || m.Blobs == nil || len(m.Entries) > gitMaterializationEntries || len(m.Blobs) > gitMaterializationEntries {
		return zero, errGitMaterialization
	}
	inv, limits := m.Inventory, DefaultGitAcquisitionBounds()
	if inv.Version != 1 || inv.SnapshotHash != Digest(expected.Snapshot) || !validAuthHash(inv.ObjectsHash) || inv.Objects < 1 || inv.Objects > limits.Objects || inv.Commits < 1 || inv.Commits > inv.Objects || inv.Commits > limits.Commits || inv.Files < 1 || inv.Files > limits.Files || inv.ExpandedBytes < 1 || inv.ExpandedBytes > limits.ExpandedBytes || inv.DiskBytes < 1 || inv.DiskBytes > limits.DiskBytes {
		return zero, errGitMaterialization
	}
	p, err := gitMaterializationPacket(packet, expected.Snapshot)
	if err != nil {
		return zero, err
	}
	blobs := map[string]bool{}
	used, previous := 0, ""
	for _, blob := range m.Blobs {
		if !objectID.MatchString(blob.Object) || len(blob.Object) != len(expected.Snapshot.Head) || blob.Object <= previous || !validAuthHash(blob.SHA256) || blob.Bytes < 0 || blob.Bytes > snapshotObjectBytes || blob.Bytes > gitMaterializationBlobBytes-used {
			return zero, errGitMaterialization
		}
		previous = blob.Object
		used += blob.Bytes
		blobs[blob.Object] = true
	}
	revisions := gitMaterializationRevisions(expected.Snapshot)
	if len(m.Entries) != len(p.Files)*len(revisions) {
		return zero, errGitMaterialization
	}
	referenced := map[string]bool{}
	at := 0
	for _, revision := range revisions {
		for _, file := range p.Files {
			e := m.Entries[at]
			at++
			if e.Revision != revision || e.Path != file.Path {
				return zero, errGitMaterialization
			}
			switch e.State {
			case "absent":
				if e.Mode != "000000" || e.Object != "" {
					return zero, errGitMaterialization
				}
			case "blob", "omitted", "nonfile":
				if !objectID.MatchString(e.Object) || len(e.Object) != len(expected.Snapshot.Head) {
					return zero, errGitMaterialization
				}
				if e.State == "nonfile" {
					if e.Mode != "120000" && e.Mode != "160000" && e.Mode != "040000" {
						return zero, errGitMaterialization
					}
				} else if e.Mode != "100644" && e.Mode != "100755" {
					return zero, errGitMaterialization
				}
				if e.State == "blob" {
					if !blobs[e.Object] {
						return zero, errGitMaterialization
					}
					referenced[e.Object] = true
				} else if e.State == "omitted" && blobs[e.Object] {
					return zero, errGitMaterialization
				}
			default:
				return zero, errGitMaterialization
			}
			// Bind the exported head/merge-base lookup to the exact admitted diff.
			mode, object := "", ""
			if revision == expected.Snapshot.Head {
				mode, object = file.NewMode, file.NewObject
			}
			if revision == expected.Snapshot.MergeBase {
				mode, object = file.OldMode, file.OldObject
			}
			if mode != "" {
				// Raw diff's zero mode means no file at this exact path. A
				// tree may occupy it during a file/directory transition, but
				// that tree must never become readable blob content.
				directory := mode == "000000" && e.State == "nonfile" && e.Mode == "040000"
				if mode == "000000" {
					object = ""
				}
				if !directory && (e.Mode != mode || e.Object != object) {
					return zero, errGitMaterialization
				}
				if file.Patch != "" && !directory && e.State != "blob" && e.State != "absent" {
					return zero, errGitMaterialization
				}
			}
		}
	}
	if len(referenced) != len(blobs) {
		return zero, errGitMaterialization
	}
	return p, nil
}

// gitMaterialization is private staging, not a qualified source capability.
// The real factory must authenticate its pipe and measured worker, bind the
// attempt and prove broker+whole-unit stop before calling restore or returning
// a JobEvidence. No public metadata/boolean constructor confers that authority.
// Sealed raw blobs need no host Git, pack/index, zlib, tree or commit parser.
type gitMaterialization struct {
	mu       sync.Mutex
	manifest gitMaterializationManifest
	packet   string
	files    map[string]*os.File
	closed   bool
}

func (m *gitMaterialization) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	var failed bool
	for id, file := range m.files {
		if file.Close() != nil {
			failed = true
		}
		delete(m.files, id)
	}
	if failed {
		return errGitMaterialization
	}
	return nil
}

// receiveGitMaterialization consumes and closes a context-cooperative private
// pipe, including exact EOF. Cancellation closes intake and joins its callback.
// All allocation lengths are checked before allocation; no remote filename or
// directory is ever opened. Every error closes every staged descriptor. An
// exact refusal record returns that bare CaptureRefusal and no materialization.
func receiveGitMaterialization(ctx context.Context, input io.ReadCloser, expected gitMaterializationBinding) (*gitMaterialization, error) {
	if input == nil {
		return nil, errGitMaterialization
	}
	defer input.Close()
	if ctx == nil || ctx.Err() != nil || !expected.valid() {
		return nil, errGitMaterialization
	}
	joined := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(joined); input.Close() })
	defer func() {
		if !stop() {
			<-joined
		}
	}()
	m := &gitMaterialization{files: map[string]*os.File{}}
	success := false
	defer func() {
		if !success {
			m.Close()
		}
	}()
	magic := make([]byte, len(gitMaterializationMagic))
	if _, err := io.ReadFull(input, magic); err != nil || string(magic) != gitMaterializationMagic && string(magic) != gitCaptureRefusalMagic {
		return nil, errGitMaterialization
	}
	frame := func(max int) ([]byte, error) {
		var size [4]byte
		if _, err := io.ReadFull(input, size[:]); err != nil {
			return nil, errGitMaterialization
		}
		n := binary.BigEndian.Uint32(size[:])
		if n == 0 || n > uint32(max) {
			return nil, errGitMaterialization
		}
		raw := make([]byte, int(n))
		if _, err := io.ReadFull(input, raw); err != nil {
			return nil, errGitMaterialization
		}
		return raw, nil
	}
	if string(magic) == gitCaptureRefusalMagic {
		// A refusal is the exact canonical record for this attempt with a known
		// reason and exact EOF. Anything else is an opaque failure.
		raw, err := frame(gitCaptureRefusalBytes)
		if err != nil {
			return nil, err
		}
		record, err := DecodeStrict[gitCaptureRefusalRecord](raw, gitCaptureRefusalBytes)
		canonical, marshalErr := json.Marshal(record)
		if err != nil || marshalErr != nil || !bytes.Equal(canonical, raw) || record.Version != 1 || record.Binding != expected {
			return nil, errGitMaterialization
		}
		var extra [1]byte
		if _, err := io.ReadFull(input, extra[:]); err != io.EOF {
			return nil, errGitMaterialization
		}
		if refusal, ok := bareCaptureRefusal(ctx, CaptureRefusal{record.Reason}); ok {
			return nil, refusal
		}
		return nil, errGitMaterialization
	}
	raw, err := frame(gitMaterializationManifestBytes)
	if err != nil {
		return nil, err
	}
	if json.Unmarshal(raw, &m.manifest) != nil {
		return nil, errGitMaterialization
	}
	canonical, err := json.Marshal(m.manifest)
	if err != nil || !bytes.Equal(canonical, raw) {
		return nil, errGitMaterialization
	}
	packet, err := frame(snapshotPacketBytes)
	if err != nil {
		return nil, err
	}
	if _, err := validateGitMaterialization(m.manifest, packet, expected); err != nil {
		return nil, err
	}
	m.packet = string(packet)
	for _, blob := range m.manifest.Blobs {
		if ctx.Err() != nil {
			return nil, errGitMaterialization
		}
		data := make([]byte, blob.Bytes)
		if _, err := io.ReadFull(input, data); err != nil || gitRawHash(data) != blob.SHA256 || gitBlobHash(data, len(blob.Object)) != blob.Object {
			return nil, errGitMaterialization
		}
		fd, err := unix.MemfdCreate("sfl-source-blob", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING|unix.MFD_NOEXEC_SEAL)
		if err != nil {
			return nil, errGitMaterialization
		}
		file := os.NewFile(uintptr(fd), "sfl-source-blob")
		m.files[blob.Object] = file
		n, err := file.Write(data)
		if err != nil || n != len(data) || unix.Fchmod(fd, 0400) != nil {
			return nil, errGitMaterialization
		}
		if _, err := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, requiredStubSeals); err != nil {
			return nil, errGitMaterialization
		}
	}
	var extra [1]byte
	if _, err := io.ReadFull(input, extra[:]); err != io.EOF || ctx.Err() != nil {
		return nil, errGitMaterialization
	}
	success = true
	return m, nil
}
func (m *gitMaterialization) blob(ctx context.Context, id string, max int) ([]byte, error) {
	if m == nil || ctx == nil || ctx.Err() != nil || max < 0 {
		return nil, errGitMaterialization
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errGitMaterialization
	}
	file := m.files[id]
	if file == nil {
		return nil, errGitMaterialization
	}
	var record *gitMaterializationBlob
	for i := range m.manifest.Blobs {
		if m.manifest.Blobs[i].Object == id {
			record = &m.manifest.Blobs[i]
			break
		}
	}
	if record == nil {
		return nil, errGitMaterialization
	}
	if record.Bytes > max {
		return nil, errGitMaterializationByteLimit
	}
	info, err := file.Stat()
	seals, sealErr := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
	if err != nil || sealErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0400 || info.Size() != int64(record.Bytes) || seals&requiredStubSeals != requiredStubSeals {
		return nil, errGitMaterialization
	}
	data := make([]byte, record.Bytes)
	n, err := file.ReadAt(data, 0)
	if err != nil && !(err == io.EOF && n == len(data)) || n != len(data) || gitRawHash(data) != record.SHA256 || gitBlobHash(data, len(id)) != id || ctx.Err() != nil {
		return nil, errGitMaterialization
	}
	return data, nil
}
func (m *gitMaterialization) Read(ctx context.Context, revision, path string, max int) (Object, error) {
	if m == nil || ctx == nil || ctx.Err() != nil || !validPath(path) || max <= 0 {
		return Object{}, errGitMaterialization
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Object{}, errGitMaterialization
	}
	var entry gitMaterializationEntry
	for _, e := range m.manifest.Entries {
		if e.Revision == revision && e.Path == path {
			entry = e
			break
		}
	}
	m.mu.Unlock()
	if entry.State == "nonfile" {
		return Object{Mode: entry.Mode}, nil
	}
	if entry.State != "blob" {
		return Object{}, errGitMaterialization
	}
	data, err := m.blob(ctx, entry.Object, max)
	if err != nil {
		return Object{}, err
	}
	return Object{entry.Mode, data}, nil
}

// restoreGitMaterialization is called only AFTER the factory has authenticated
// the worker/attempt and proved whole-unit/broker stop. It is private plumbing,
// not the missing factory's qualification check. Existing Git-backed offline
// helpers remain for trusted fixtures and compatibility; the active controller
// must select this path for acquired source.
func restoreGitMaterialization(ctx context.Context, m *gitMaterialization, job Job) (*JobEvidence, error) {
	if m == nil || ctx == nil || ctx.Err() != nil {
		return nil, errGitMaterialization
	}
	m.mu.Lock()
	if m.closed || job.Snapshot != m.manifest.Binding.Snapshot || job.Packet != m.packet {
		m.mu.Unlock()
		return nil, errGitMaterialization
	}
	p, err := validateGitMaterialization(m.manifest, []byte(m.packet), m.manifest.Binding)
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	inventory := append([]snapshotFile(nil), p.Files...)
	for i := range inventory {
		inventory[i].Patch = ""
	}
	return restoreSnapshotEvidencePacket(ctx, m, job, inventory, func(id string, remaining *int) ([]byte, error) {
		if strings.Trim(id, "0") == "" {
			return nil, nil
		}
		data, err := m.blob(ctx, id, *remaining)
		if err == nil {
			*remaining -= len(data)
		}
		return data, err
	})
}
