//go:build linux

package reviewer

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func nativePipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { read.Close(); write.Close() })
	return read, write
}

func donatedCodePipes(t *testing.T, source, sink *os.File) []*os.File {
	t.Helper()
	donor, broker := vmSocketPair(t)
	done := make(chan error, 1)
	go func() {
		_, _, err := donor.WriteMsgUnix([]byte(nativeCodeHandoff), unix.UnixRights(int(source.Fd()), int(sink.Fd())), nil)
		if err == nil {
			err = donor.CloseWrite()
		}
		done <- err
	}()
	files, err := receiveNativeCodePipes(broker)
	if err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		closeVMFiles(files)
		t.Fatal(err)
	}
	donor.Close()
	broker.Close() // Launcher loss has no completion authority.
	t.Cleanup(func() { closeVMFiles(files) })
	for _, file := range files {
		flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
		if err != nil || flags&unix.FD_CLOEXEC == 0 {
			t.Fatal("donated pipe can escape an exec")
		}
	}
	return files
}

func TestNativeOwnedForwardingSurvivesLauncherDescriptorClosure(t *testing.T) {
	source, clientInput := nativePipe(t)
	clientOutput, sink := nativePipe(t)
	childInput, childSink := nativePipe(t)
	childSource, childOutput := nativePipe(t)
	files := donatedCodePipes(t, source, sink)
	source.Close()
	sink.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan [2]nativeCodeStream, 1)
	go func() {
		in, out := nativeCodeForward(ctx, files[0], files[1], childSink, childSource, nil)
		closeVMFiles(files)
		done <- [2]nativeCodeStream{in, out}
	}()
	childDone := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(childInput)
		io.WriteString(childOutput, "response")
		childOutput.Close()
		childDone <- string(data)
	}()
	io.WriteString(clientInput, "request")
	clientInput.Close()
	response, err := io.ReadAll(clientOutput)
	result := <-done
	if err != nil || string(response) != "response" || <-childDone != "request" || result[0].err != nil || result[1].err != nil || result[0].hash != authHash([]byte("request")) || result[1].hash != authHash([]byte("response")) {
		t.Fatal("launcher closure lost trusted forwarding proof", result, err)
	}
}

func TestNativeOwnedForwardingCancellationStopsBlockedCopies(t *testing.T) {
	source, _ := nativePipe(t)
	_, sink := nativePipe(t)
	_, childSink := nativePipe(t)
	childSource, _ := nativePipe(t)
	files := donatedCodePipes(t, source, sink)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan [2]nativeCodeStream, 1)
	go func() {
		in, out := nativeCodeForward(ctx, files[0], files[1], childSink, childSource, nil)
		done <- [2]nativeCodeStream{in, out}
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case result := <-done:
		if result[0].err == nil && result[1].err == nil {
			t.Fatal("cancelled forwarding attested completion")
		}
	case <-time.After(time.Second):
		t.Fatal("owned pipe copies survived cancellation")
	}
}

func TestNativeOwnedForwardingPropagatesChildEOFBeforeClientInputCloses(t *testing.T) {
	source, clientInput := nativePipe(t)
	clientOutput, sink := nativePipe(t)
	_, childSink := nativePipe(t)
	childSource, childOutput := nativePipe(t)
	files := donatedCodePipes(t, source, sink)
	source.Close()
	sink.Close()
	childOutput.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		nativeCodeForward(ctx, files[0], files[1], childSink, childSource, nil)
		closeVMFiles(files)
		close(done)
	}()
	clientOutput.SetReadDeadline(time.Now().Add(time.Second))
	var data [1]byte
	_, err := clientOutput.Read(data[:])
	clientInput.Close()
	<-done
	if err != io.EOF {
		t.Fatal("client never observed the pinned child's EOF", err)
	}
}

func TestNativeCodePipeHandoffRejectsFilesSocketsAliasesAndWrongDirection(t *testing.T) {
	source, sourceWrite := nativePipe(t)
	_, sink := nativePipe(t)
	regular, err := os.Create(filepath.Join(t.TempDir(), "not-a-pipe"))
	if err != nil {
		t.Fatal(err)
	}
	defer regular.Close()
	socket, _ := vmSocketPair(t)
	socketFile, err := socket.File()
	if err != nil {
		t.Fatal(err)
	}
	defer socketFile.Close()
	for _, files := range [][]*os.File{nil, {source}, {regular, sink}, {socketFile, sink}, {sourceWrite, sink}, {source, source}, {source, sourceWrite}} {
		if nativeCodePipes(files) == nil {
			t.Fatal("unsafe pipe pair accepted")
		}
	}
	if nativeCodePipes([]*os.File{source, sink}) != nil {
		t.Fatal("distinct anonymous stdio pipes rejected")
	}
}

func TestNativeCodePipeHandoffRejectsEOFAndOldOrIncompleteFrames(t *testing.T) {
	for _, kind := range []string{"eof", "old", "missing", "extra"} {
		t.Run(kind, func(t *testing.T) {
			source, _ := nativePipe(t)
			_, sink := nativePipe(t)
			donor, broker := vmSocketPair(t)
			done := make(chan struct{})
			go func() {
				defer close(done)
				if kind != "eof" {
					raw := nativeCodeHandoff
					fds := []int{int(source.Fd()), int(sink.Fd())}
					if kind == "old" {
						raw = "SFL_CODEHOST_STDIO_2\n"
					}
					if kind == "missing" {
						fds = fds[:1]
					}
					if kind == "extra" {
						fds = append(fds, int(source.Fd()))
					}
					donor.WriteMsgUnix([]byte(raw), unix.UnixRights(fds...), nil)
				}
				donor.CloseWrite()
			}()
			files, err := receiveNativeCodePipes(broker)
			<-done
			if err == nil || len(files) != 0 {
				closeVMFiles(files)
				t.Fatal("invalid launcher frame acquired relay authority", kind)
			}
		})
	}
}
