//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

type vmObjectStoreFunc func(context.Context, string, string, int) (Object, error)

func (f vmObjectStoreFunc) Read(c context.Context, r, p string, m int) (Object, error) {
	return f(c, r, p, m)
}

func TestVMObjectsActualUnixProducerAndBoundedImmutableStore(t *testing.T) {
	client, server := vmSocketPair(t)
	s := testSession(Codex)
	s.Limits.ReaderBytes = 10
	s.Limits.ReaderCalls = 5
	requests := make([]int, 0)
	calls := 0
	store := vmObjectStoreFunc(func(_ context.Context, revision, path string, limit int) (Object, error) {
		calls++
		requests = append(requests, limit)
		if revision != s.Snapshot.Head {
			t.Error("revision changed")
		}
		if path == "missing.go" {
			return Object{}, errors.New("private-host-path-secret")
		}
		return Object{"100644", []byte("hello")}, nil
	})
	done := make(chan error, 1)
	go func() {
		done <- ServeVMObjects(context.Background(), server, s, []string{"safe.go", "missing.go"}, store)
	}()
	stream, err := NewVMObjectStream(client, s)
	if err != nil {
		t.Fatal(err)
	}
	object, err := stream.Read(context.Background(), s.Snapshot.Head, "safe.go", 10)
	if err != nil || object.Mode != "100644" || string(object.Data) != "hello" {
		t.Fatal(object, err)
	}
	if _, err = stream.Read(context.Background(), s.Snapshot.Head, "missing.go", 10); err != ErrVMObjects {
		t.Fatal("private store error forwarded", err)
	}
	object, err = stream.Read(context.Background(), s.Snapshot.Head, "safe.go", 10)
	if err != nil || string(object.Data) != "hello" {
		t.Fatal("bounded remaining read failed", object, err)
	}
	if _, err = stream.Read(context.Background(), s.Snapshot.Head, "safe.go", 10); err != ErrSourceByteLimit {
		t.Fatal("exhausted host budget returned data or hid the byte limit", err)
	}
	stream.Close()
	if err = <-done; err != nil {
		t.Fatal("clean frame-boundary EOF rejected", err)
	}
	if calls != 3 || len(requests) != 3 || requests[0] != 10 || requests[1] != 5 || requests[2] != 5 {
		t.Fatal("store bytes/calls exceeded captured limits", calls, requests)
	}
}

// The guest reader records a host byte-budget refusal as the byte limit, not as
// a missing object, and the stream stays usable for later in-budget reads.
func TestVMObjectsCarryByteBudgetRefusalToTheGuestReader(t *testing.T) {
	client, server := vmSocketPair(t)
	s := testSession(Codex)
	s.Limits.ReaderBytes = 10
	s.Limits.ReaderCalls = 5
	store := vmObjectStoreFunc(func(_ context.Context, _, path string, limit int) (Object, error) {
		switch path {
		case "big.go":
			// Deliberately ignores limit: the object stream owns the bound.
			return Object{"100644", []byte("0123456789a")}, nil
		case "capped.go":
			return Object{}, fmt.Errorf("private-host-path-secret: %w", ErrSourceByteLimit)
		case "safe.go":
			return Object{"100644", []byte("hello")}, nil
		}
		return Object{}, errors.New("private-host-path-secret")
	})
	paths := []string{"big.go", "capped.go", "safe.go", "gone.go"}
	done := make(chan error, 1)
	go func() { done <- ServeVMObjects(context.Background(), server, s, paths, store) }()
	stream, err := NewVMObjectStream(client, s)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(stream, s.Snapshot, paths, s.Limits)
	if err != nil {
		t.Fatal(err)
	}
	head := s.Snapshot.Head
	if _, err = reader.Read(context.Background(), head, "big.go"); err == nil {
		t.Fatal("oversized object served")
	}
	if _, err = reader.Read(context.Background(), head, "capped.go"); err == nil {
		t.Fatal("store-capped object served")
	}
	if _, err = reader.Read(context.Background(), head, "gone.go"); err == nil {
		t.Fatal("missing object served")
	}
	if text, err := reader.Read(context.Background(), head, "safe.go"); err != nil || text != "hello" {
		t.Fatal("stream unusable after a byte-budget refusal", text, err)
	}
	reads, coverage := reader.Evidence()
	want := []Coverage{{"big.go", "source byte limit exhausted"}, {"capped.go", "source byte limit exhausted"}, {"gone.go", "missing or unreadable object"}}
	if len(reads) != 1 || fmt.Sprint(coverage) != fmt.Sprint(want) {
		t.Fatal("byte budget refusal not told apart from a missing object", reads, coverage)
	}
	stream.Close()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestVMObjectsRejectForeignScopeBeforeStoreReads(t *testing.T) {
	for _, kind := range []string{"session", "revision", "path", "sequence", "bytes", "unknown", "duplicate", "partial", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			client, server := vmSocketPair(t)
			s := testSession(Codex)
			calls := 0
			done := make(chan error, 1)
			go func() {
				done <- ServeVMObjects(context.Background(), server, s, []string{"safe.go"}, vmObjectStoreFunc(func(context.Context, string, string, int) (Object, error) {
					calls++
					return Object{"100644", []byte("safe")}, nil
				}))
			}()
			q := vmObjectRequest{1, Digest(s), 1, s.Snapshot.Head, "safe.go", 10}
			switch kind {
			case "session":
				q.SessionHash = Digest("foreign")
			case "revision":
				q.Revision = strings.Repeat("f", 40)
			case "path":
				q.Path = "../outside"
			case "sequence":
				q.Sequence = 2
			case "bytes":
				q.MaxBytes = s.Limits.ReaderBytes + 1
			}
			var frame bytes.Buffer
			if kind == "oversize" {
				var header [4]byte
				binary.BigEndian.PutUint32(header[:], 2049)
				frame.Write(header[:])
			} else if kind == "partial" {
				frame.Write([]byte{0, 0})
			} else {
				raw, _ := json.Marshal(q)
				if kind == "unknown" {
					raw = append(raw[:len(raw)-1], []byte(`,"Command":"sh"}`)...)
				}
				if kind == "duplicate" {
					raw = append(raw[:len(raw)-1], []byte(`,"Version":1}`)...)
				}
				var header [4]byte
				binary.BigEndian.PutUint32(header[:], uint32(len(raw)))
				frame.Write(header[:])
				frame.Write(raw)
			}
			client.Write(frame.Bytes())
			client.CloseWrite()
			if err := <-done; err != ErrVMObjects || calls != 0 {
				t.Fatal("foreign request reached immutable store", kind, calls, err)
			}
		})
	}
}

func TestVMObjectStreamRejectsForgedReplyAndNeverReplays(t *testing.T) {
	for _, kind := range []string{"session", "sequence", "oversized-object", "denied-data", "limit-without-denial", "limit-with-data", "noncanonical"} {
		t.Run(kind, func(t *testing.T) {
			client, server := vmSocketPair(t)
			s := testSession(Codex)
			stream, err := NewVMObjectStream(client, s)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			done := make(chan error, 1)
			go func() {
				q, err := readVMRecord[vmObjectRequest](server, 2048)
				if err != nil {
					done <- err
					return
				}
				reply := vmObjectReply{Version: 1, SessionHash: q.SessionHash, Sequence: q.Sequence, Object: Object{"100644", []byte("safe")}}
				switch kind {
				case "session":
					reply.SessionHash = Digest("foreign")
				case "sequence":
					reply.Sequence++
				case "oversized-object":
					reply.Object.Data = make([]byte, 11)
				case "denied-data":
					reply.Denied = true
				case "limit-without-denial":
					reply.ByteLimit = true
				case "limit-with-data":
					reply.Denied, reply.ByteLimit = true, true
				}
				if kind == "noncanonical" {
					raw, _ := json.Marshal(reply)
					raw = append(raw[:len(raw)-1], []byte(`,"Version":1}`)...)
					var header [4]byte
					binary.BigEndian.PutUint32(header[:], uint32(len(raw)))
					if _, err = server.Write(append(header[:], raw...)); err != nil {
						done <- err
						return
					}
				} else {
					if err = writeVMRecord(server, reply, 4<<20); err != nil {
						done <- err
						return
					}
				}
				var extra [1]byte
				n, err := server.Read(extra[:])
				if n != 0 || err != io.EOF {
					done <- ErrVMObjects
					return
				}
				done <- nil
			}()
			if _, err = stream.Read(context.Background(), s.Snapshot.Head, "safe.go", 10); err != ErrVMObjects {
				t.Fatal("forged private reply accepted", kind, err)
			}
			if _, err = stream.Read(context.Background(), s.Snapshot.Head, "safe.go", 10); err != ErrVMObjects {
				t.Fatal("failed stream retried", err)
			}
			if err = <-done; err != nil {
				t.Fatal("failed stream sent more bytes", err)
			}
		})
	}
}

func TestVMObjectsCancellationClosesBothRolesWithoutStoreErrorDisclosure(t *testing.T) {
	client, server := vmSocketPair(t)
	s := testSession(Codex)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- ServeVMObjects(ctx, server, s, []string{"safe.go"}, vmObjectStoreFunc(func(ctx context.Context, _, _ string, _ int) (Object, error) {
			close(entered)
			<-ctx.Done()
			return Object{}, errors.New("private-store-secret")
		}))
	}()
	stream, err := NewVMObjectStream(client, s)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	readDone := make(chan error, 1)
	go func() { _, err := stream.Read(ctx, s.Snapshot.Head, "safe.go", 10); readDone <- err }()
	<-entered
	cancel()
	if err = <-readDone; err != ErrVMObjects {
		t.Fatal("cancelled reader exposed internal error", err)
	}
	if err = <-done; err != ErrVMObjects {
		t.Fatal("cancelled server did not stop", err)
	}
}
