//go:build linux

package reviewer

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type gitKernelFileID struct{ Device, Inode uint64 }
type gitExecutableMap struct {
	File       gitKernelFileID
	KernelName string
	Writable   bool
}
type gitProcessNetwork struct {
	Interfaces                []string
	IPv4Routes, IPv6Routes    int
	IPv6Rejects, IPv6Loopback int
}
type gitProcessObservation struct {
	Identity                 GateIdentity
	ParentPID                int
	Root, Executable         gitKernelFileID
	RootFlags                int64
	Namespaces               map[string]gitNamespaceIdentity
	Capabilities             map[string]uint64
	UIDs, GIDs               [4]uint32
	NoNewPrivileges, Seccomp int
	Cgroup                   string
	ExecutableMaps           []gitExecutableMap
	Network                  gitProcessNetwork
}

func gitStatFileID(file *os.File) (gitKernelFileID, error) {
	if file == nil {
		return gitKernelFileID{}, errGitUnit
	}
	var stat unix.Stat_t
	if unix.Fstat(int(file.Fd()), &stat) != nil || stat.Ino == 0 {
		return gitKernelFileID{}, errGitUnit
	}
	return gitKernelFileID{uint64(stat.Dev), stat.Ino}, nil
}

// This reads current kernel evidence for an ORIGINAL retained peer pidfd. It
// never reopens that peer's numeric PID. Numeric proc paths are used only while
// the original pidfd/start/boot identity is live before and after observation.
// The result is diagnostic evidence, not reusable admission authority. The
// composing factory must freshly establish unit/cgroup, ancestry, private
// namespaces, capabilities, maps, descriptors, routes and policy before grant.
func observeGitProcess(ctx context.Context, identity GateIdentity, original int, expectedRoot, expectedExecutable *os.File, pin VMAssetPin) (gitProcessObservation, error) {
	zero := gitProcessObservation{}
	if ctx == nil || ctx.Err() != nil || !gitRuntimePin(pin, 64<<20) {
		return zero, errGitUnit
	}
	pidfd, err := gitRetainProcessPIDFD(identity, original)
	if err != nil {
		return zero, errGitUnit
	}
	defer unix.Close(pidfd)
	root, err := duplicateGitControlFile(expectedRoot)
	if err != nil {
		return zero, errGitUnit
	}
	defer root.Close()
	expected, err := duplicateGitControlFile(expectedExecutable)
	if err != nil {
		return zero, errGitUnit
	}
	defer expected.Close()
	rootID, err := gitStatFileID(root)
	if err != nil {
		return zero, errGitUnit
	}
	exeID, err := gitStatFileID(expected)
	if err != nil {
		return zero, errGitUnit
	}
	procPath := fmt.Sprintf("/proc/%d", identity.PID)
	proc, err := os.OpenRoot(procPath)
	if err != nil {
		return zero, errGitUnit
	}
	defer proc.Close()
	read := func(name string, max int64) ([]byte, error) {
		if ctx.Err() != nil {
			return nil, errGitUnit
		}
		file, err := openRootNoFollow(proc, name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return nil, errGitUnit
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, max+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || int64(len(raw)) > max || ctx.Err() != nil {
			return nil, errGitUnit
		}
		return raw, nil
	}
	stat, err := read("stat", 4096)
	if err != nil {
		return zero, errGitUnit
	}
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return zero, errGitUnit
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 20 {
		return zero, errGitUnit
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil || parent < 0 {
		return zero, errGitUnit
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || start != identity.StartTicks {
		return zero, errGitUnit
	}
	observation := gitProcessObservation{Identity: identity, ParentPID: parent, Namespaces: map[string]gitNamespaceIdentity{}, Capabilities: map[string]uint64{}}
	actualRoot, err := os.Open(procPath + "/root")
	if err != nil {
		return zero, errGitUnit
	}
	actualRootID, rootErr := gitStatFileID(actualRoot)
	var fs unix.Statfs_t
	fsErr := unix.Fstatfs(int(actualRoot.Fd()), &fs)
	actualRoot.Close()
	if rootErr != nil || fsErr != nil || actualRootID != rootID {
		return zero, errGitUnit
	}
	observation.Root, observation.RootFlags = actualRootID, fs.Flags
	executable, err := os.Open(procPath + "/exe")
	if err != nil {
		return zero, errGitUnit
	}
	actualExe, statErr := gitStatFileID(executable)
	info, infoErr := executable.Stat()
	if statErr != nil || infoErr != nil || actualExe != exeID || !info.Mode().IsRegular() || info.Size() != pin.Bytes {
		executable.Close()
		return zero, errGitUnit
	}
	hash, hashErr := gitBootstrapImageHash(ctx, executable, pin.Bytes)
	executable.Close()
	if hashErr != nil || hash != pin.SHA256 {
		return zero, errGitUnit
	}
	observation.Executable = actualExe
	status, err := read("status", 64<<10)
	if err != nil {
		return zero, errGitUnit
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(status), "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		selected := key == "Uid" || key == "Gid" || key == "NoNewPrivs" || key == "Seccomp" || key == "CapInh" || key == "CapPrm" || key == "CapEff" || key == "CapBnd" || key == "CapAmb"
		if !selected {
			continue
		}
		if seen[key] {
			return zero, errGitUnit
		}
		seen[key] = true
		parts := strings.Fields(value)
		switch key {
		case "Uid", "Gid":
			if len(parts) != 4 {
				return zero, errGitUnit
			}
			for index, part := range parts {
				id, err := strconv.ParseUint(part, 10, 32)
				if err != nil {
					return zero, errGitUnit
				}
				if key == "Uid" {
					observation.UIDs[index] = uint32(id)
				} else {
					observation.GIDs[index] = uint32(id)
				}
			}
		case "NoNewPrivs", "Seccomp":
			if len(parts) != 1 {
				return zero, errGitUnit
			}
			value, err := strconv.Atoi(parts[0])
			if err != nil || value < 0 || value > 2 {
				return zero, errGitUnit
			}
			if key == "NoNewPrivs" {
				if value > 1 {
					return zero, errGitUnit
				}
				observation.NoNewPrivileges = value
			} else {
				observation.Seccomp = value
			}
		default:
			if len(parts) != 1 || len(parts[0]) != 16 {
				return zero, errGitUnit
			}
			value, err := strconv.ParseUint(parts[0], 16, 64)
			if err != nil {
				return zero, errGitUnit
			}
			observation.Capabilities[key] = value
		}
	}
	if len(seen) != 9 || len(observation.Capabilities) != 5 {
		return zero, errGitUnit
	}
	group, err := read("cgroup", 4096)
	if err != nil {
		return zero, errGitUnit
	}
	line := strings.TrimSuffix(string(group), "\n")
	if !strings.HasPrefix(line, "0::/") || strings.ContainsAny(line, "\n\r\x00") {
		return zero, errGitUnit
	}
	observation.Cgroup = strings.TrimPrefix(line, "0::")
	devices, err := read("net/dev", 64<<10)
	if err != nil {
		return zero, errGitUnit
	}
	deviceLines := strings.Split(strings.TrimSuffix(string(devices), "\n"), "\n")
	if len(deviceLines) < 3 || !strings.HasPrefix(deviceLines[0], "Inter-") || !strings.HasPrefix(strings.TrimSpace(deviceLines[1]), "face") {
		return zero, errGitUnit
	}
	for _, line := range deviceLines[2:] {
		name, counters, found := strings.Cut(line, ":")
		name = strings.TrimSpace(name)
		parts := strings.Fields(counters)
		if !found || name == "" || len(name) > 15 || len(parts) != 16 || len(observation.Network.Interfaces) >= 256 {
			return zero, errGitUnit
		}
		for _, part := range parts {
			if _, err := strconv.ParseUint(part, 10, 64); err != nil {
				return zero, errGitUnit
			}
		}
		observation.Network.Interfaces = append(observation.Network.Interfaces, name)
	}
	routes, err := read("net/route", 64<<10)
	if err != nil {
		return zero, errGitUnit
	}
	routeLines := strings.Split(strings.TrimSuffix(string(routes), "\n"), "\n")
	if len(routeLines) == 0 || !strings.HasPrefix(routeLines[0], "Iface\tDestination\tGateway") {
		return zero, errGitUnit
	}
	for _, line := range routeLines[1:] {
		if strings.TrimSpace(line) != "" {
			observation.Network.IPv4Routes++
		}
	}
	routes, err = read("net/ipv6_route", 64<<10)
	if err != nil {
		return zero, errGitUnit
	}
	if gitObserveIPv6Routes(routes, &observation.Network) != nil {
		return zero, errGitUnit
	}
	for name, kind := range map[string]int{"user": unix.CLONE_NEWUSER, "mnt": unix.CLONE_NEWNS, "pid": unix.CLONE_NEWPID, "net": unix.CLONE_NEWNET, "ipc": unix.CLONE_NEWIPC, "uts": unix.CLONE_NEWUTS} {
		file, err := os.Open(procPath + "/ns/" + name)
		if err != nil {
			return zero, errGitUnit
		}
		typeID, typeErr := unix.IoctlRetInt(int(file.Fd()), unix.NS_GET_NSTYPE)
		id, statErr := gitStatFileID(file)
		file.Close()
		if typeErr != nil || statErr != nil || typeID != kind {
			return zero, errGitUnit
		}
		observation.Namespaces[name] = gitNamespaceIdentity{id.Device, id.Inode}
	}
	maps, err := read("maps", 128<<10)
	if err != nil {
		return zero, errGitUnit
	}
	for _, line := range strings.Split(string(maps), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 5 || len(parts[1]) != 4 {
			return zero, errGitUnit
		}
		if parts[1][2] != 'x' {
			continue
		}
		device := strings.Split(parts[3], ":")
		if len(device) != 2 {
			return zero, errGitUnit
		}
		major, e1 := strconv.ParseUint(device[0], 16, 32)
		minor, e2 := strconv.ParseUint(device[1], 16, 32)
		inode, e3 := strconv.ParseUint(parts[4], 10, 64)
		if e1 != nil || e2 != nil || e3 != nil {
			return zero, errGitUnit
		}
		entry := gitExecutableMap{File: gitKernelFileID{unix.Mkdev(uint32(major), uint32(minor)), inode}, Writable: parts[1][1] == 'w'}
		if inode == 0 && len(parts) == 6 && (parts[5] == "[vdso]" || parts[5] == "[vsyscall]") {
			entry.KernelName = parts[5]
		}
		observation.ExecutableMaps = append(observation.ExecutableMaps, entry)
		if len(observation.ExecutableMaps) > 256 {
			return zero, errGitUnit
		}
	}
	if len(observation.ExecutableMaps) == 0 || ctx.Err() != nil || gitCheckProcessPIDFD(identity, pidfd) != nil {
		return zero, errGitUnit
	}
	return observation, nil
}

// Linux emits reject defaults and ::1/128 even in this freshly isolated
// namespace. Only those exact loopback-local forms are non-egress evidence.
// Unknown route forms are counted, never silently discarded as harmless.
func gitObserveIPv6Routes(raw []byte, network *gitProcessNetwork) error {
	if network == nil || len(raw) > 64<<10 {
		return errGitUnit
	}
	const zero = "00000000000000000000000000000000"
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 10 || len(parts[9]) == 0 || len(parts[9]) > 15 {
			return errGitUnit
		}
		for _, index := range []int{0, 2, 4} {
			if !gitQuarantineHex(parts[index], 32) {
				return errGitUnit
			}
		}
		for _, index := range []int{1, 3} {
			if !gitQuarantineHex(parts[index], 2) {
				return errGitUnit
			}
			n, err := strconv.ParseUint(parts[index], 16, 8)
			if err != nil || n > 128 {
				return errGitUnit
			}
		}
		for _, index := range []int{5, 6, 7, 8} {
			if !gitQuarantineHex(parts[index], 8) {
				return errGitUnit
			}
		}
		local := parts[2] == zero && parts[3] == "00" && parts[4] == zero && parts[9] == "lo"
		switch {
		case local && parts[0] == zero && parts[1] == "00" && parts[5] == "ffffffff" && parts[8] == "00200200":
			network.IPv6Rejects++
		case local && parts[0] == "00000000000000000000000000000001" && parts[1] == "80" && parts[5] == "00000000" && parts[8] == "80200001":
			network.IPv6Loopback++
		default:
			network.IPv6Routes++
		}
	}
	return nil
}
