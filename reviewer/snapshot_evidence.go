package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const snapshotPacketBytes = 64 << 10
const snapshotFileLimit = 128
const snapshotObjectBytes = 2 << 20

// SnapshotEvidence is captured by the trusted source reader, never supplied by
// a model. Its methods return copies; packet, reader paths and inline anchors
// all describe the same immutable merge-base-to-head comparison.
type SnapshotEvidence struct {
	store    ObjectStore
	snapshot Snapshot
	packet   string
	paths    []string
	coverage []Coverage
	lines    map[string]map[int]bool
}

// JobEvidence binds reader coverage and diff anchors to the packet actually
// admitted by SQLite. An idempotent Admit can return an older captured packet;
// a recapture with different omissions must not silently replace its evidence.
type JobEvidence struct {
	evidence *SnapshotEvidence
	limits   Limits
	jobHash  string
}

func (e *SnapshotEvidence) Bind(job Job) (*JobEvidence, error) {
	if e == nil || job.Snapshot != e.snapshot || job.Packet != e.packet || job.Validate() != nil {
		return nil, ErrFenced
	}
	return &JobEvidence{evidence: e, limits: job.Config.Limits, jobHash: Digest(job)}, nil
}

func (bound *JobEvidence) validateJob(job Job) error {
	if bound == nil || bound.evidence == nil || job.Validate() != nil || bound.jobHash != Digest(job) {
		return ErrFenced
	}
	return nil
}

func snapshotEvidencePacket(packet string) bool {
	var header struct{ Version string }
	return json.Unmarshal([]byte(packet), &header) == nil && header.Version == "sfl-snapshot-evidence-1"
}

// Render retains the admitted-job binding through inline eligibility checks.
func (bound *JobEvidence) Render(out Outcome) (Document, error) {
	if err := bound.validateJob(out.Job); err != nil {
		return Document{}, err
	}
	return renderOutcome(out, bound.diffLines())
}

func (p Publisher) PrepareEvidence(ctx context.Context, out Outcome, bound *JobEvidence) (WriteIntent, error) {
	if err := bound.validateJob(out.Job); err != nil {
		return WriteIntent{}, err
	}
	return p.prepare(ctx, out, bound.diffLines())
}

type snapshotFile struct {
	Path, Status, OldMode, NewMode, OldObject, NewObject string
	Patch                                                string `json:",omitempty"`
}

type snapshotPacket struct {
	Version  string
	Snapshot Snapshot
	Files    []snapshotFile
	Coverage []Coverage
}

func (e *SnapshotEvidence) Packet() string  { return e.packet }
func (e *SnapshotEvidence) Paths() []string { return append([]string(nil), e.paths...) }
func (e *SnapshotEvidence) Coverage() []Coverage {
	return append([]Coverage(nil), e.coverage...)
}
func (bound *JobEvidence) diffLines() map[string]map[int]bool {
	e := bound.evidence
	out := map[string]map[int]bool{}
	for path, lines := range e.lines {
		out[path] = map[int]bool{}
		for line := range lines {
			out[path][line] = true
		}
	}
	return out
}

// Reader supplies independent budgets while retaining controller-observed
// omissions. It does not grant source access outside this captured path list.
func (bound *JobEvidence) Reader(snapshot Snapshot) (*Reader, error) {
	if bound == nil || bound.evidence == nil {
		return nil, ErrFenced
	}
	e := bound.evidence
	if snapshot != e.snapshot {
		return nil, ErrFenced
	}
	r, err := NewReader(e.store, snapshot, e.paths, bound.limits)
	if err != nil {
		return nil, err
	}
	r.initialCoverage = e.Coverage()
	r.coverage = e.Coverage()
	return r, nil
}

// CaptureSnapshotEvidence runs only read-only Git plumbing against an explicitly
// supplied trusted object store. Missing history defers the whole capture. It
// never fetches, checks out a tree, reads the working tree or executes its code.
// Whole changed-path inventories must fit; individual source/patch omissions
// remain explicit. No partially collected packet is returned on failure.
func CaptureSnapshotEvidence(ctx context.Context, git GitStore, snapshot Snapshot) (*SnapshotEvidence, error) {
	if snapshot.Validate() != nil {
		return nil, ErrIgnored
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	files, err := snapshotInventory(ctx, git, snapshot)
	if err != nil {
		return nil, err
	}
	e := &SnapshotEvidence{store: git, snapshot: snapshot, lines: map[string]map[int]bool{}}
	p := snapshotPacket{Version: "sfl-snapshot-evidence-1", Snapshot: snapshot, Files: files, Coverage: []Coverage{}}
	remaining := snapshotObjectBytes
	for i := range p.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file := &p.Files[i]
		e.paths = append(e.paths, file.Path)
		omit := func(reason string) { p.Coverage = append(p.Coverage, Coverage{file.Path, reason}) }
		if (file.OldMode != "000000" && file.OldMode != "100644" && file.OldMode != "100755") || (file.NewMode != "000000" && file.NewMode != "100644" && file.NewMode != "100755") {
			omit("symlink, submodule or non-file diff omitted")
			continue
		}
		old, err := snapshotBlob(ctx, git, file.OldObject, &remaining)
		if err != nil {
			omit("old source unavailable or source byte limit exhausted")
			continue
		}
		new, err := snapshotBlob(ctx, git, file.NewObject, &remaining)
		if err != nil {
			omit("new source unavailable or source byte limit exhausted")
			continue
		}
		if bytes.IndexByte(old, 0) >= 0 || bytes.IndexByte(new, 0) >= 0 || !utf8.Valid(old) || !utf8.Valid(new) {
			omit("binary or non-UTF-8 diff omitted")
			continue
		}
		args := []string{"-c", "diff.suppressBlankEmpty=false", "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--ignore-submodules=none", "--no-color", "--no-prefix", "--text", "--diff-algorithm=myers", "--no-indent-heuristic", "--unified=3", snapshot.MergeBase, snapshot.Head, "--", ":(literal)" + file.Path}
		// A literal directory pathspec still selects descendants. A file/tree
		// transition has separate raw records for those descendants; exclude
		// them here so this patch reproduces only this record's old/new blobs.
		for _, other := range p.Files {
			if strings.HasPrefix(other.Path, file.Path+"/") {
				args = append(args, ":(literal,exclude)"+other.Path)
			}
		}
		patch, err := git.command(ctx, snapshotObjectBytes, args...)
		if err != nil {
			omit("patch unavailable or patch byte limit exhausted")
			continue
		}
		lines, err := snapshotHunkLines(string(patch), string(old), string(new))
		if err != nil {
			return nil, errors.New("patch does not reproduce immutable source")
		}
		file.Patch = string(patch)
		encoded, _ := json.Marshal(p)
		if len(encoded) > snapshotPacketBytes {
			file.Patch = ""
			omit("patch omitted by evidence packet byte limit")
			continue
		}
		e.lines[file.Path] = lines
	}
	encoded, err := fitSnapshotPacket(&p, e.lines, snapshotPacketBytes)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.packet = string(encoded)
	e.coverage = append([]Coverage(nil), p.Coverage...)
	return e, nil
}

func snapshotInventory(ctx context.Context, git GitStore, snapshot Snapshot) ([]snapshotFile, error) {
	for _, id := range []string{snapshot.Base, snapshot.Head, snapshot.MergeBase} {
		kind, err := git.command(ctx, 32, "cat-file", "-t", id)
		if err != nil || string(kind) != "commit\n" {
			return nil, errors.New("snapshot commit history unavailable")
		}
	}
	ancestor, err := git.command(ctx, 256, "merge-base", "--all", snapshot.Base, snapshot.Head)
	if err != nil || string(ancestor) != snapshot.MergeBase+"\n" {
		return nil, errors.New("snapshot merge-base history unavailable or ambiguous")
	}
	args := []string{"diff", "--raw", "-z", "--no-abbrev", "--no-renames", "--ignore-submodules=none", "--no-ext-diff", "--no-textconv", "--no-color", snapshot.MergeBase, snapshot.Head, "--"}
	raw, err := git.command(ctx, snapshotObjectBytes, args...)
	if err != nil {
		return nil, errors.New("complete changed-path inventory unavailable")
	}
	files, err := parseSnapshotChanges(raw, len(snapshot.Head))
	if err != nil {
		return nil, err
	}
	return files, nil
}

// RestoreSnapshotEvidence reconstructs the admitted packet against its immutable
// Git inventory. Originally omitted patches stay omitted even if their objects
// become available later. Included patches must still reproduce their blobs;
// unavailable included evidence defers recovery rather than changing the job.
func RestoreSnapshotEvidence(ctx context.Context, git GitStore, job Job) (*JobEvidence, error) {
	if job.Validate() != nil || len(job.Packet) > snapshotPacketBytes {
		return nil, ErrFenced
	}
	var p snapshotPacket
	if json.Unmarshal([]byte(job.Packet), &p) != nil {
		return nil, ErrFenced
	}
	canonical, err := json.Marshal(p)
	if err != nil || string(canonical) != job.Packet || p.Version != "sfl-snapshot-evidence-1" || p.Snapshot != job.Snapshot {
		return nil, ErrFenced
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	inventory, err := snapshotInventory(ctx, git, job.Snapshot)
	if err != nil {
		return nil, err
	}
	if len(p.Files) != len(inventory) {
		return nil, ErrFenced
	}
	return restoreSnapshotEvidencePacket(ctx, git, job, inventory, func(id string, remaining *int) ([]byte, error) {
		return snapshotBlob(ctx, git, id, remaining)
	})
}

// restoreSnapshotEvidencePacket parses only bounded JSON, text and plain blob
// bytes. The caller owns inventory/source qualification. It preserves every
// admitted omission and never recaptures a packet during recovery.
func restoreSnapshotEvidencePacket(ctx context.Context, store ObjectStore, job Job, inventory []snapshotFile, blob func(string, *int) ([]byte, error)) (*JobEvidence, error) {
	if job.Validate() != nil || len(job.Packet) > snapshotPacketBytes || store == nil || blob == nil {
		return nil, ErrFenced
	}
	var p snapshotPacket
	if json.Unmarshal([]byte(job.Packet), &p) != nil {
		return nil, ErrFenced
	}
	canonical, err := json.Marshal(p)
	if err != nil || string(canonical) != job.Packet || p.Version != "sfl-snapshot-evidence-1" || p.Snapshot != job.Snapshot || len(p.Files) != len(inventory) {
		return nil, ErrFenced
	}
	omissions := map[string]string{}
	for _, c := range p.Coverage {
		if _, duplicate := omissions[c.Path]; duplicate {
			return nil, ErrFenced
		}
		switch c.Reason {
		case "symlink, submodule or non-file diff omitted", "old source unavailable or source byte limit exhausted", "new source unavailable or source byte limit exhausted", "binary or non-UTF-8 diff omitted", "patch unavailable or patch byte limit exhausted", "patch omitted by evidence packet byte limit":
		default:
			return nil, ErrFenced
		}
		omissions[c.Path] = c.Reason
	}
	e := &SnapshotEvidence{store: store, snapshot: job.Snapshot, packet: job.Packet, coverage: append([]Coverage(nil), p.Coverage...), lines: map[string]map[int]bool{}}
	remaining := snapshotObjectBytes
	for i, file := range p.Files {
		identity := file
		identity.Patch = ""
		if identity != inventory[i] {
			return nil, ErrFenced
		}
		e.paths = append(e.paths, file.Path)
		_, omitted := omissions[file.Path]
		if file.Patch == "" {
			if !omitted {
				return nil, ErrFenced
			}
			delete(omissions, file.Path)
			continue
		}
		regular := func(mode string) bool { return mode == "000000" || mode == "100644" || mode == "100755" }
		if omitted || !regular(file.OldMode) || !regular(file.NewMode) {
			return nil, ErrFenced
		}
		old, err := blob(file.OldObject, &remaining)
		if err != nil {
			return nil, err
		}
		new, err := blob(file.NewObject, &remaining)
		if err != nil {
			return nil, err
		}
		if bytes.IndexByte(old, 0) >= 0 || bytes.IndexByte(new, 0) >= 0 || !utf8.Valid(old) || !utf8.Valid(new) {
			return nil, ErrFenced
		}
		lines, err := snapshotHunkLines(file.Patch, string(old), string(new))
		if err != nil {
			return nil, ErrFenced
		}
		e.lines[file.Path] = lines
	}
	if len(omissions) != 0 {
		return nil, ErrFenced
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return e.Bind(job)
}

func snapshotBlob(ctx context.Context, git GitStore, id string, remaining *int) ([]byte, error) {
	if strings.Trim(id, "0") == "" {
		return nil, nil
	}
	if *remaining <= 0 {
		return nil, ErrDeferred
	}
	// Inspect size first so an oversized blob cannot repeatedly consume the
	// capture's aggregate read budget through capped failed cat-file calls.
	raw, err := git.command(ctx, 32, "cat-file", "-s", id)
	size, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || parseErr != nil || size < 0 || size > *remaining {
		return nil, ErrDeferred
	}
	*remaining -= size
	data, err := git.command(ctx, max(1, size), "cat-file", "blob", id)
	if err != nil || len(data) != size {
		return nil, ErrDeferred
	}
	return data, nil
}

func parseSnapshotChanges(raw []byte, hashLength int) ([]snapshotFile, error) {
	files := []snapshotFile{}
	if len(raw) == 0 {
		return files, nil
	}
	if raw[len(raw)-1] != 0 {
		return nil, ErrDeferred
	}
	parts := bytes.Split(raw[:len(raw)-1], []byte{0})
	if len(parts)%2 != 0 || len(parts)/2 > snapshotFileLimit {
		return nil, errors.New("changed-path inventory is malformed or exceeds file limit")
	}
	seen := map[string]bool{}
	for i := 0; i < len(parts); i += 2 {
		fields := strings.Split(string(parts[i]), " ")
		path := string(parts[i+1])
		if len(fields) != 5 || !strings.HasPrefix(fields[0], ":") || !validPath(path) || seen[path] {
			return nil, errors.New("changed-path inventory contains unsafe or duplicate entries")
		}
		f := snapshotFile{Path: path, OldMode: strings.TrimPrefix(fields[0], ":"), NewMode: fields[1], OldObject: fields[2], NewObject: fields[3], Status: fields[4]}
		validMode := func(s string) bool {
			return s == "000000" || s == "100644" || s == "100755" || s == "120000" || s == "160000"
		}
		oldZero, newZero := strings.Trim(f.OldObject, "0") == "", strings.Trim(f.NewObject, "0") == ""
		if !validMode(f.OldMode) || !validMode(f.NewMode) || !objectID.MatchString(f.OldObject) || !objectID.MatchString(f.NewObject) || len(f.OldObject) != hashLength || len(f.NewObject) != hashLength || oldZero != (f.OldMode == "000000") || newZero != (f.NewMode == "000000") {
			return nil, ErrDeferred
		}
		if (f.Status == "A" && (!oldZero || newZero)) || (f.Status == "D" && (oldZero || !newZero)) || ((f.Status == "M" || f.Status == "T") && (oldZero || newZero)) || (f.Status != "A" && f.Status != "D" && f.Status != "M" && f.Status != "T") {
			return nil, ErrDeferred
		}
		seen[path] = true
		files = append(files, f)
	}
	return files, nil
}

var snapshotHunk = regexp.MustCompile(`^@@ -([0-9]+)(?:,([0-9]+))? \+([0-9]+)(?:,([0-9]+))? @@(?: .*)?$`)

func snapshotSourceLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// The patch's filenames and free-form function labels never select a path or
// line. Every consumed context/add/delete line must match the retained blobs.
func snapshotHunkLines(patch, old, new string) (map[int]bool, error) {
	result := map[int]bool{}
	oldLines, newLines := snapshotSourceLines(old), snapshotSourceLines(new)
	oldAt, newAt, oldEnd, newEnd := 0, 0, 0, 0
	hunks := 0
	needsEOFMarker := false
	lines := strings.Split(strings.TrimSuffix(patch, "\n"), "\n")
	for _, line := range lines {
		if needsEOFMarker {
			if line != "\\ No newline at end of file" {
				return nil, ErrDeferred
			}
			needsEOFMarker = false
			continue
		}
		if strings.HasPrefix(line, "@@") {
			m := snapshotHunk.FindStringSubmatch(line)
			if m == nil || oldAt != oldEnd || newAt != newEnd {
				return nil, ErrDeferred
			}
			values := [4]int{}
			for i := range values {
				if m[i+1] == "" {
					values[i] = 1
				} else {
					v, err := strconv.Atoi(m[i+1])
					if err != nil || v < 0 || v > snapshotObjectBytes {
						return nil, ErrDeferred
					}
					values[i] = v
				}
			}
			os, oc, ns, nc := values[0], values[1], values[2], values[3]
			if oc > 0 {
				os--
			}
			if nc > 0 {
				ns--
			}
			if os < oldEnd || ns < newEnd || os < 0 || ns < 0 || oc > len(oldLines)-os || nc > len(newLines)-ns || (oc == 0 && nc == 0) {
				return nil, ErrDeferred
			}
			// Unchanged gaps must also agree; an omitted real edit cannot hide
			// behind a later hunk whose local text happens to be valid.
			if strings.Join(oldLines[oldEnd:os], "\n") != strings.Join(newLines[newEnd:ns], "\n") || os-oldEnd != ns-newEnd {
				return nil, ErrDeferred
			}
			oldAt, newAt, oldEnd, newEnd = os, ns, os+oc, ns+nc
			hunks++
			continue
		}
		if hunks == 0 {
			continue
		} // Git metadata before the first hunk only.
		if len(line) == 0 {
			return nil, ErrDeferred
		}
		prefix, text := line[0], line[1:]
		if prefix != ' ' && prefix != '+' && prefix != '-' {
			return nil, ErrDeferred
		}
		if prefix == ' ' && oldAt == len(oldLines)-1 && newAt == len(newLines)-1 && strings.HasSuffix(old, "\n") != strings.HasSuffix(new, "\n") {
			return nil, ErrDeferred
		}
		if prefix != '+' {
			if oldAt >= oldEnd || oldLines[oldAt] != text {
				return nil, ErrDeferred
			}
			oldAt++
			needsEOFMarker = oldAt == len(oldLines) && !strings.HasSuffix(old, "\n")
		}
		if prefix != '-' {
			if newAt >= newEnd || newLines[newAt] != text {
				return nil, ErrDeferred
			}
			newAt++
			needsEOFMarker = needsEOFMarker || (newAt == len(newLines) && !strings.HasSuffix(new, "\n"))
			result[newAt] = true
		}
	}
	if needsEOFMarker || (hunks == 0 && old != new) || (oldEnd < len(oldLines) && strings.HasSuffix(old, "\n") != strings.HasSuffix(new, "\n")) || oldAt != oldEnd || newAt != newEnd || len(oldLines)-oldEnd != len(newLines)-newEnd || strings.Join(oldLines[oldEnd:], "\n") != strings.Join(newLines[newEnd:], "\n") {
		return nil, fmt.Errorf("incomplete source patch: %w", ErrDeferred)
	}
	return result, nil
}

// fitSnapshotPacket preserves small valid patches when an omission record
// would cost more bytes. Every accepted retirement strictly reduces the packet.
func fitSnapshotPacket(p *snapshotPacket, inline map[string]map[int]bool, max int) ([]byte, error) {
	encoded, err := json.Marshal(p)
	for i := len(p.Files) - 1; err == nil && len(encoded) > max && i >= 0; i-- {
		file := &p.Files[i]
		if file.Patch == "" {
			continue
		}
		patch := file.Patch
		file.Patch = ""
		coverageLen := len(p.Coverage)
		p.Coverage = append(p.Coverage, Coverage{file.Path, "patch omitted by evidence packet byte limit"})
		smaller, marshalErr := json.Marshal(p)
		if marshalErr != nil || len(smaller) >= len(encoded) {
			file.Patch = patch
			p.Coverage = p.Coverage[:coverageLen]
			if marshalErr != nil {
				return nil, marshalErr
			}
			continue
		}
		delete(inline, file.Path)
		encoded = smaller
	}
	if err != nil || len(encoded) > max {
		return nil, errors.New("complete evidence inventory and coverage exceed packet limit")
	}
	return encoded, nil
}
