package reviewer

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

var ErrVMObjects = errors.New("private immutable object channel rejected")

// VMObjectStream is supplied only to the trusted guest reader role. Its channel
// must be private, authenticated by the native launch, and closed on shutdown.
// It is not an MCP endpoint or a model-accessible repository selector.
type VMObjectStream struct {
	mu       sync.Mutex
	channel  io.ReadWriteCloser
	session  string
	sequence int
	failed   bool
}

type vmObjectRequest struct {
	Version        int
	SessionHash    string
	Sequence       int
	Revision, Path string
	MaxBytes       int
}
type vmObjectReply struct {
	Version     int
	SessionHash string
	Sequence    int
	Denied      bool
	// ByteLimit marks a denial by the byte budget, not an unavailable object.
	// It is valid only on a denied reply.
	ByteLimit bool
	Object    Object
}

func NewVMObjectStream(channel io.ReadWriteCloser, s Session) (*VMObjectStream, error) {
	if channel == nil || s.Validate() != nil {
		return nil, ErrVMObjects
	}
	return &VMObjectStream{channel: channel, session: Digest(s)}, nil
}

// Native private records are finite canonical JSON frames. Length is checked
// before allocation. This helper is also used by the guest control channel.
func writeVMRecord(w io.Writer, value any, limit int) error {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) == 0 || len(raw) > limit {
		return ErrVMObjects
	}
	defer clear(raw)
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(raw)))
	for _, part := range [][]byte{prefix[:], raw} {
		for len(part) > 0 {
			n, err := w.Write(part)
			if n < 0 || n > len(part) || err != nil || n == 0 {
				return ErrVMObjects
			}
			part = part[n:]
		}
	}
	return nil
}
func readVMRecord[T any](r io.Reader, limit int) (T, error) {
	var value T
	var prefix [4]byte
	if n, err := io.ReadFull(r, prefix[:]); err != nil {
		if n == 0 && err == io.EOF {
			return value, io.EOF
		}
		return value, ErrVMObjects
	}
	size := int64(binary.BigEndian.Uint32(prefix[:]))
	if size <= 0 || size > int64(limit) {
		return value, ErrVMObjects
	}
	raw := make([]byte, int(size))
	defer clear(raw)
	if _, err := io.ReadFull(r, raw); err != nil {
		return value, ErrVMObjects
	}
	// Exact re-encoding rejects duplicates, unknown fields, alternate spelling,
	// null/omitted required scalar encodings and extra JSON values.
	if json.Unmarshal(raw, &value) != nil {
		return value, ErrVMObjects
	}
	canonical, err := json.Marshal(value)
	if err != nil || !bytes.Equal(canonical, raw) {
		return value, ErrVMObjects
	}
	return value, nil
}

func (s *VMObjectStream) Read(ctx context.Context, revision, path string, maxBytes int) (Object, error) {
	if s == nil || s.channel == nil {
		return Object{}, ErrVMObjects
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed || ctx.Err() != nil || !objectID.MatchString(revision) || !validPath(path) || maxBytes <= 0 || maxBytes > DefaultLimits().ReaderBytes {
		return Object{}, ErrVMObjects
	}
	// A failed/partial exchange permanently consumes the stream. No replay on a
	// reconnect or mismatched reply can substitute a different immutable object.
	s.failed = true
	defer func() {
		if s.failed {
			s.channel.Close()
		}
	}()
	stop := context.AfterFunc(ctx, func() { s.channel.Close() })
	defer stop()
	s.sequence++
	q := vmObjectRequest{1, s.session, s.sequence, revision, path, maxBytes}
	if writeVMRecord(s.channel, q, 2048) != nil {
		return Object{}, ErrVMObjects
	}
	reply, err := readVMRecord[vmObjectReply](s.channel, 4<<20)
	if err != nil || ctx.Err() != nil || reply.Version != 1 || reply.SessionHash != s.session || reply.Sequence != s.sequence || len(reply.Object.Data) > maxBytes || len(reply.Object.Mode) > 6 || (reply.Denied && (reply.Object.Mode != "" || len(reply.Object.Data) != 0)) || (reply.ByteLimit && !reply.Denied) {
		return Object{}, ErrVMObjects
	}
	s.failed = false
	if reply.ByteLimit {
		return Object{}, ErrSourceByteLimit
	}
	if reply.Denied {
		return Object{}, ErrVMObjects
	}
	return reply.Object, nil
}
func (s *VMObjectStream) Close() error {
	if s == nil || s.channel == nil {
		return nil
	}
	return s.channel.Close()
}

// ServeVMObjects reads only captured revisions/paths through the controller's
// trusted immutable store. It enforces an independent call/byte budget before
// forwarding anything and returns no store errors or host paths over the wire.
// Object bytes never share the control/auth or provider stream.
func ServeVMObjects(ctx context.Context, channel io.ReadWriteCloser, s Session, paths []string, store ObjectStore) error {
	if channel == nil || s.Validate() != nil || store == nil || len(paths) > 4096 {
		return ErrVMObjects
	}
	defer channel.Close()
	allowed := map[string]bool{}
	for _, path := range paths {
		if !validPath(path) || allowed[path] {
			return ErrVMObjects
		}
		allowed[path] = true
	}
	stop := context.AfterFunc(ctx, func() { channel.Close() })
	defer stop()
	used, sequence := 0, 0
	sessionHash := Digest(s)
	for {
		q, err := readVMRecord[vmObjectRequest](channel, 2048)
		if err != nil {
			if err == io.EOF && ctx.Err() == nil {
				return nil
			}
			return ErrVMObjects
		}
		sequence++
		if ctx.Err() != nil || q.Version != 1 || q.SessionHash != sessionHash || q.Sequence != sequence || sequence > s.Limits.ReaderCalls || !allowed[q.Path] || !validPath(q.Path) || (q.Revision != s.Snapshot.Head && q.Revision != s.Snapshot.Base && q.Revision != s.Snapshot.MergeBase) || q.MaxBytes <= 0 || q.MaxBytes > s.Limits.ReaderBytes {
			return ErrVMObjects
		}
		maximum := min(q.MaxBytes, s.Limits.ReaderBytes-used)
		reply := vmObjectReply{Version: 1, SessionHash: sessionHash, Sequence: sequence, Denied: true, ByteLimit: maximum <= 0}
		if maximum > 0 {
			object, err := store.Read(ctx, q.Revision, q.Path, maximum)
			if err == nil && len(object.Data) <= maximum && len(object.Mode) <= 6 {
				reply.Object = object
				reply.Denied = false
				used += len(object.Data)
			} else {
				// Only the budget outcome crosses the channel, never the store
				// error or any byte of the refused object.
				reply.ByteLimit = err == nil && len(object.Data) > maximum || errors.Is(err, ErrSourceByteLimit)
			}
		}
		if ctx.Err() != nil || writeVMRecord(channel, reply, 4<<20) != nil {
			return ErrVMObjects
		}
	}
}
