//go:build linux

package reviewer

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const gitMountInfoBytes = 64 << 10

type gitMountRow struct {
	ID, Parent                uint64
	Device                    uint64
	Root, Point               string
	Filesystem                string
	Options, Super            map[string]bool
	Shared, Master, Propagate uint64
	Unbindable                bool
}

func gitMountPath(raw string) (string, error) {
	var result strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			result.WriteByte(raw[i])
			continue
		}
		if i+3 >= len(raw) {
			return "", errGitUnit
		}
		switch raw[i+1 : i+4] {
		case "040":
			result.WriteByte(' ')
		case "134":
			result.WriteByte('\\')
		default:
			// Control-character pathnames are outside the fixed launch layout.
			return "", errGitUnit
		}
		i += 3
	}
	value := result.String()
	if len(value) > 4096 || !filepath.IsAbs(value) || filepath.Clean(value) != value || strings.ContainsAny(value, "\x00\r\n\t") {
		return "", errGitUnit
	}
	return value, nil
}

func gitMountOptions(raw string) (map[string]bool, error) {
	options := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		if part == "" || len(part) > 4096 || options[part] {
			return nil, errGitUnit
		}
		options[part] = true
	}
	if options["ro"] == options["rw"] {
		return nil, errGitUnit
	}
	return options, nil
}

// Kernel mountinfo only. Parsing never confers admission or source authority.
func parseGitMountTable(raw []byte) (map[string]gitMountRow, error) {
	if len(raw) == 0 || len(raw) > gitMountInfoBytes || raw[len(raw)-1] != '\n' || strings.ContainsAny(string(raw), "\x00\r\t") {
		return nil, errGitUnit
	}
	rows := map[string]gitMountRow{}
	ids := map[uint64]bool{}
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		left, right, found := strings.Cut(line, " - ")
		a, b := strings.Fields(left), strings.Fields(right)
		if !found || len(a) < 6 || len(a) > 10 || len(b) != 3 || len(rows) >= 32 {
			return nil, errGitUnit
		}
		id, e1 := strconv.ParseUint(a[0], 10, 64)
		parent, e2 := strconv.ParseUint(a[1], 10, 64)
		device := strings.Split(a[2], ":")
		if e1 != nil || e2 != nil || id == 0 || parent == 0 || ids[id] || len(device) != 2 {
			return nil, errGitUnit
		}
		major, e1 := strconv.ParseUint(device[0], 10, 32)
		minor, e2 := strconv.ParseUint(device[1], 10, 32)
		root, e3 := gitMountPath(a[3])
		point, e4 := gitMountPath(a[4])
		options, e5 := gitMountOptions(a[5])
		super, e6 := gitMountOptions(b[2])
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil || e6 != nil || rows[point].ID != 0 || b[0] == "" {
			return nil, errGitUnit
		}
		row := gitMountRow{ID: id, Parent: parent, Device: unix.Mkdev(uint32(major), uint32(minor)), Root: root, Point: point, Filesystem: b[0], Options: options, Super: super}
		seen := map[string]bool{}
		for _, field := range a[6:] {
			key, value, found := strings.Cut(field, ":")
			if seen[key] {
				return nil, errGitUnit
			}
			seen[key] = true
			if key == "unbindable" && !found {
				row.Unbindable = true
				continue
			}
			valueID, err := strconv.ParseUint(value, 10, 64)
			if !found || err != nil || valueID == 0 {
				return nil, errGitUnit
			}
			switch key {
			case "shared":
				row.Shared = valueID
			case "master":
				row.Master = valueID
			case "propagate_from":
				row.Propagate = valueID
			default:
				return nil, errGitUnit
			}
		}
		if row.Unbindable && (row.Shared != 0 || row.Master != 0 || row.Propagate != 0) || row.Propagate != 0 && row.Master == 0 {
			return nil, errGitUnit
		}
		rows[point] = row
		ids[id] = true
	}
	return rows, nil
}

func validateGitMountLayout(rows map[string]gitMountRow, binding gitMaterializationBinding) error {
	if !binding.valid() || len(rows) != 7 || rows["/"].ID == 0 {
		return errGitUnit
	}
	root := rows["/"]
	wanted := map[string]bool{"/": true, "/broker": true, "/work": true, "/tmp": true, "/proc": true, "/dev/null": true, "/dev/urandom": true}
	for point, row := range rows {
		if !wanted[point] || point != "/" && row.Parent != root.ID || row.Shared != 0 {
			return errGitUnit
		}
		if point == "/" || point == "/broker" || point == "/proc" || point == "/dev/urandom" {
			if !row.Options["ro"] {
				return errGitUnit
			}
		}
	}
	if rows["/proc"].Filesystem != "proc" || rows["/proc"].Root != "/" || !rows["/dev/null"].Options["rw"] {
		return errGitUnit
	}
	for _, point := range []string{"/work", "/tmp"} {
		row := rows[point]
		if row.Filesystem != "tmpfs" || row.Root != "/git-work-"+binding.Attempt || !row.Options["rw"] || !row.Options["nosuid"] || !row.Options["nodev"] || !row.Options["noexec"] || !row.Super["noswap"] {
			return errGitUnit
		}
	}
	if rows["/work"].Device != rows["/tmp"].Device {
		return errGitUnit
	}
	return nil
}

func gitReadMountInfo(ctx context.Context, identity GateIdentity, pidfd int) ([]byte, error) {
	if ctx == nil || ctx.Err() != nil || gitCheckProcessPIDFD(identity, pidfd) != nil {
		return nil, errGitUnit
	}
	file, err := os.Open(fmt.Sprintf("/proc/%d/mountinfo", identity.PID))
	if err != nil {
		return nil, errGitUnit
	}
	data, readErr := io.ReadAll(io.LimitReader(file, gitMountInfoBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(data) > gitMountInfoBytes || ctx.Err() != nil || gitCheckProcessPIDFD(identity, pidfd) != nil {
		return nil, errGitUnit
	}
	return data, nil
}

func gitBoundedWorkFilesystem(fs unix.Statfs_t) bool {
	const size = uint64(268435456)
	flags := int64(unix.ST_NOSUID | unix.ST_NODEV | unix.ST_NOEXEC)
	return fs.Type == unix.TMPFS_MAGIC && fs.Bsize > 0 && size%uint64(fs.Bsize) == 0 && fs.Blocks == size/uint64(fs.Bsize) && fs.Files == 65536 && fs.Flags&flags == flags && fs.Flags&unix.ST_RDONLY == 0
}

// This proves only the mount/device subset for an already retained process
// boundary. The factory must still bind the exact unit, peer, descriptors and
// installed filter, and must never turn this private result into a caller flag.
func checkGitMountBoundary(ctx context.Context, b *gitProcessBoundary, broker *os.File, binding gitMaterializationBinding) error {
	return checkGitMountBoundaryIDs(ctx, b, broker, binding, uint32(os.Geteuid()), 0700)
}

func checkGitMountBoundaryIDs(ctx context.Context, b *gitProcessBoundary, broker *os.File, binding gitMaterializationBinding, brokerOwner uint32, brokerMode os.FileMode) error {
	if ctx == nil || b == nil || broker == nil || !binding.valid() {
		return errGitUnit
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.checkWorker(ctx) != nil || verifyGitBrokerDirectoryIDs(broker, brokerOwner, b.uid, brokerMode) != nil {
		return errGitUnit
	}
	raw, err := gitReadMountInfo(ctx, b.worker, b.pidfd)
	if err != nil {
		return errGitUnit
	}
	rows, err := parseGitMountTable(raw)
	if err != nil || validateGitMountLayout(rows, binding) != nil {
		return errGitUnit
	}
	procRoot := fmt.Sprintf("/proc/%d/root", b.worker.PID)
	view, err := os.OpenRoot(procRoot)
	if err != nil {
		return errGitUnit
	}
	defer view.Close()
	ids := map[string]gitKernelFileID{}
	for _, point := range []string{"/", "/broker", "/work", "/tmp", "/proc", "/dev/null", "/dev/urandom"} {
		name := strings.TrimPrefix(point, "/")
		if name == "" {
			name = "."
		}
		file, err := view.OpenFile(name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return errGitUnit
		}
		var stat unix.Stat_t
		statErr := unix.Fstat(int(file.Fd()), &stat)
		id, idErr := gitStatFileID(file)
		file.Close()
		if statErr != nil || idErr != nil || uint64(stat.Dev) != rows[point].Device {
			return errGitUnit
		}
		ids[point] = id
		if point == "/dev/null" || point == "/dev/urandom" {
			minor := uint32(3)
			if point == "/dev/urandom" {
				minor = 9
			}
			if stat.Mode&unix.S_IFMT != unix.S_IFCHR || stat.Uid != 0 || stat.Rdev != unix.Mkdev(1, minor) {
				return errGitUnit
			}
		} else if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			return errGitUnit
		}
		if point == "/work" || point == "/tmp" {
			if stat.Uid != b.uid || stat.Mode&0777 != 0700 {
				return errGitUnit
			}
			file, err := view.OpenFile(name, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
			if err != nil {
				return errGitUnit
			}
			var fs unix.Statfs_t
			fsErr := unix.Fstatfs(int(file.Fd()), &fs)
			file.Close()
			if fsErr != nil || !gitBoundedWorkFilesystem(fs) {
				return errGitUnit
			}
		}
	}
	rootID, rootErr := gitStatFileID(b.root)
	brokerID, brokerErr := gitStatFileID(broker)
	if rootErr != nil || brokerErr != nil || ids["/"] != rootID || ids["/broker"] != brokerID || ids["/work"] != ids["/tmp"] {
		return errGitUnit
	}
	pidNamespace, err := os.Open(procRoot + "/proc/1/ns/pid")
	if err != nil {
		return errGitUnit
	}
	id, idErr := gitStatFileID(pidNamespace)
	kind, kindErr := unix.IoctlRetInt(int(pidNamespace.Fd()), unix.NS_GET_NSTYPE)
	pidNamespace.Close()
	if idErr != nil || kindErr != nil || kind != unix.CLONE_NEWPID || gitNamespaceIdentity(id) != b.namespaces["pid"] {
		return errGitUnit
	}
	after, err := gitReadMountInfo(ctx, b.worker, b.pidfd)
	if err != nil || string(after) != string(raw) || verifyGitBrokerDirectory(broker) != nil || b.checkWorker(ctx) != nil {
		return errGitUnit
	}
	return nil
}
