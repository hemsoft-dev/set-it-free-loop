//go:build linux

package reviewer

import (
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

func nativeGuestBootStage(stage string) {
	if os.Getpid() == 1 && os.Getuid() == 0 && nativeBootStageValid(stage) {
		fmt.Fprintln(os.Stderr, "SFL_BOOT_STAGE="+stage)
	}
}

// Kernel/QEMU bytes never become retained logs. The bounded streaming parser
// retains only a short transient matching tail and fixed diagnostic enums.
type nativeBootDiagnostics struct {
	mu         sync.Mutex
	tail       []byte
	stage      string
	panic, oom bool
	scanned    int
}

func (d *nativeBootDiagnostics) Write(data []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.scanned >= 1<<20 {
		return len(data), nil
	}
	part := data[:min(len(data), (1<<20)-d.scanned)]
	d.scanned += len(part)
	joined := append(append([]byte{}, d.tail...), part...)
	text := strings.ReplaceAll(string(joined), "\r\n", "\n")
	latest := -1
	for _, stage := range nativeBootStages {
		if index := strings.LastIndex(text, "SFL_BOOT_STAGE="+stage+"\n"); index > latest {
			latest, d.stage = index, stage
		}
	}
	d.panic = d.panic || strings.Contains(text, "Kernel panic")
	d.oom = d.oom || strings.Contains(text, "Out of memory") || strings.Contains(text, "out of memory")
	clear(d.tail)
	d.tail = append([]byte{}, joined[max(0, len(joined)-128):]...)
	clear(joined)
	return len(data), nil
}

func (d *nativeBootDiagnostics) stages(failed bool) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	defer func() { clear(d.tail); d.tail = nil }()
	result := []string{}
	if d.stage != "" {
		result = append(result, "diag-"+d.stage)
	}
	if d.oom {
		result = append(result, "diag-oom")
	}
	if d.panic {
		result = append(result, "diag-panic")
	}
	if len(result) == 0 {
		if failed {
			result = append(result, "diag-vm-error")
		} else {
			result = append(result, "diag-vm-exit")
		}
	}
	return result
}

func nativeBootDiagnosticChannel() (net.Conn, *os.File, error) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, ErrVMBoot
	}
	parent := os.NewFile(uintptr(pair[0]), "private-boot-diagnostic")
	child := os.NewFile(uintptr(pair[1]), "private-boot-diagnostic-child")
	connection, err := net.FileConn(parent)
	parent.Close()
	if err != nil {
		child.Close()
		return nil, nil, ErrVMBoot
	}
	return connection, child, nil
}

func readNativeBootDiagnostics(input io.Reader, diagnostics *nativeBootDiagnostics) {
	buffer := make([]byte, 4096)
	defer clear(buffer)
	for {
		n, err := input.Read(buffer)
		if n > 0 {
			diagnostics.Write(buffer[:n])
			clear(buffer[:n])
		}
		if err != nil || n == 0 {
			return
		}
	}
}
