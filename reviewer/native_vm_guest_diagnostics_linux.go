//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
)

// Only enums leave this scanner. It bounds scan work, retains at most a small
// cross-write suffix, and clears that suffix after classification. Raw stderr
// never reaches a transcript, report or host file.
type nativeGuestClientDiagnostics struct {
	tail    []byte
	scanned int
	flags   [7]bool
}

var nativeGuestDiagnosticWords = [][]string{{"certificate", "x509", "tls"}, {"proxy"}, {"auth", "token"}, {"permission denied", "operation not permitted", "no such file", "read-only"}, {"invalid", "unknown", "configuration"}, {"panic", "runtime"}}

func (d *nativeGuestClientDiagnostics) Write(input []byte) (int, error) {
	accepted := len(input)
	for len(input) > 0 && d.scanned < 64<<10 {
		n := min(len(input), 2048, (64<<10)-d.scanned)
		chunk := make([]byte, 0, len(d.tail)+n)
		chunk = append(chunk, d.tail...)
		chunk = append(chunk, input[:n]...)
		lower := bytes.ToLower(chunk)
		for index, words := range nativeGuestDiagnosticWords {
			for _, word := range words {
				if bytes.Contains(lower, []byte(word)) {
					d.flags[index] = true
				}
			}
		}
		clear(d.tail)
		d.tail = append(d.tail[:0], chunk[max(0, len(chunk)-128):]...)
		clear(chunk)
		clear(lower)
		d.scanned += n
		input = input[n:]
	}
	return accepted, nil
}

func (d *nativeGuestClientDiagnostics) exitCode(failure error) int {
	defer clear(d.tail)
	if errors.Is(failure, context.Canceled) || errors.Is(failure, context.DeadlineExceeded) {
		return 209
	}
	if failure != nil {
		var exited *exec.ExitError
		if !errors.As(failure, &exited) {
			return 208
		}
		if exited.ExitCode() < 0 {
			return 210
		}
	}
	if d.scanned == 0 {
		return 211
	}
	for index, seen := range d.flags {
		if seen {
			return 201 + index
		}
	}
	return 207
}

func (p *nativeGuestPhase) observeClientExit() {
	if p.client == nil {
		return
	}
	select {
	case <-p.client.done:
	default:
		return
	}
	var exit *exec.ExitError
	if !errors.As(p.client.waitErr, &exit) {
		return
	}
	classes := map[int]string{201: "tls", 202: "proxy", 203: "auth", 204: "filesystem", 205: "configuration", 206: "runtime", 207: "other", 208: "launch", 209: "cancelled", 210: "signal", 211: "silent-exit", 221: "tls-authority", 222: "tls-intake", 223: "tls-handshake", 224: "tls-host"}
	class := classes[exit.ExitCode()]
	if class == "" {
		return
	}
	p.nativeMu.Lock()
	defer p.nativeMu.Unlock()
	if p.native != nil {
		p.native.ExitClass = class
	}
}
