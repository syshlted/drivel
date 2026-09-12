package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/zishmusic/drivel/plugin/internal/pb"
)

// The Content service is the one call that runs from the plugin back to the
// host, and it exists for exactly one reason: provider.RangePutter takes an
// io.ReaderAt.
//
// That is not an accident of the Go interface — it is the contract. An
// implementation seeks to each extent in whatever order its wire protocol
// prefers, which is what lets a backend write extents as its own protocol likes
// them rather than as drivel happened to list them. Streaming the local file
// into PutRange would replace random access with a single forward pass and
// quietly change the contract for every backend that runs as a plugin, while
// leaving it intact for every backend that does not — the worst possible
// outcome, because the in-process form is what the backend's own tests use.
//
// So the host serves the file instead, and the surface is kept as narrow as two
// decisions can make it:
//
//   - **One channel per process, not per call.** go-plugin's AcceptAndServe
//     returns only when the whole broker shuts down, so a listener started for
//     one PutRange would outlive it and accumulate one per range write for the
//     life of the mount. The channel is opened at Open and lives as long as the
//     connection does.
//   - **A handle per call, valid only during it.** The plugin cannot name a path
//     — it can only ask for bytes of a file the host registered immediately
//     before the call and dropped immediately after. That is the difference
//     between "the plugin may read the file the host chose" and "the plugin may
//     read files".

// contentRegistry holds the files currently readable by the plugin: at most one
// per in-flight PutRange.
type contentRegistry struct {
	mu    sync.Mutex
	next  uint64
	files map[uint64]sizedReader
}

type sizedReader struct {
	src  io.ReaderAt
	size int64
}

func newContentRegistry() *contentRegistry {
	return &contentRegistry{files: map[uint64]sizedReader{}}
}

// add makes src readable and returns the handle naming it, plus the function
// that stops it being readable. The caller must always call the second.
func (r *contentRegistry) add(src io.ReaderAt, size int64) (uint64, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Handles start at 1 so that a zero-valued request — an older plugin, a bug —
	// can never name a live file.
	r.next++
	id := r.next
	r.files[id] = sizedReader{src: src, size: size}
	return id, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.files, id)
	}
}

func (r *contentRegistry) get(id uint64) (sizedReader, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.files[id]
	return f, ok
}

// contentServer answers ReadAt against whichever file the handle names.
type contentServer struct {
	pb.UnimplementedContentServer
	reg *contentRegistry
}

func (c *contentServer) ReadAt(_ context.Context, req *pb.ReadAtRequest) (*pb.ReadAtResponse, error) {
	f, ok := c.reg.get(req.GetHandle())
	if !ok {
		// Either the call it belonged to has returned, or the plugin invented one.
		// Neither is retryable and neither should be described vaguely.
		return nil, encodeError(fmt.Errorf("content: handle %d is not open", req.GetHandle()))
	}
	n := req.GetLen()
	if n <= 0 {
		return &pb.ReadAtResponse{}, nil
	}
	if n > chunkSize {
		n = chunkSize
	}
	off := req.GetOff()
	// A read starting at or beyond the end is EOF rather than an error: it is how
	// a caller discovers where the file stops, and io.ReaderAt implementations
	// disagree about whether they report it themselves.
	if off >= f.size {
		return &pb.ReadAtResponse{Eof: true}, nil
	}
	if rem := f.size - off; int64(n) > rem {
		n = int32(rem)
	}
	buf := make([]byte, n)
	read, err := f.src.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, encodeError(err)
	}
	return &pb.ReadAtResponse{
		Data: buf[:read],
		Eof:  errors.Is(err, io.EOF) || off+int64(read) >= f.size,
	}, nil
}

// serveContent builds the gRPC server the broker runs the Content service on.
func serveContent(reg *contentRegistry) func([]grpc.ServerOption) *grpc.Server {
	return func(opts []grpc.ServerOption) *grpc.Server {
		s := grpc.NewServer(append(opts,
			grpc.MaxRecvMsgSize(maxMessage),
			grpc.MaxSendMsgSize(maxMessage),
		)...)
		pb.RegisterContentServer(s, &contentServer{reg: reg})
		return s
	}
}

// contentDialer is the plugin side: it dials the host's Content service once,
// the first time a range write needs it, and keeps the connection.
//
// Lazily, because a backend that does not implement RangePutter must not pay for
// a channel it will never use — and because that is most of them.
type contentDialer struct {
	broker   *goplugin.GRPCBroker
	brokerID uint32

	mu     sync.Mutex
	client pb.ContentClient
	conn   io.Closer
}

func (d *contentDialer) dial() (pb.ContentClient, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.client != nil {
		return d.client, nil
	}
	if d.brokerID == 0 {
		return nil, errors.New("plugin: the host advertised no content service, so range writes are not available")
	}
	conn, err := d.broker.Dial(d.brokerID)
	if err != nil {
		return nil, fmt.Errorf("plugin: dialling the host's content service: %w", err)
	}
	d.client, d.conn = pb.NewContentClient(conn), conn
	return d.client, nil
}

func (d *contentDialer) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn != nil {
		_ = d.conn.Close()
		d.conn, d.client = nil, nil
	}
}

// brokerReaderAt is the plugin-side io.ReaderAt over one handle.
//
// The context is the PutRange call's, deliberately: the file is readable for as
// long as the call patching it is running, and not one moment longer.
type brokerReaderAt struct {
	client pb.ContentClient
	ctx    context.Context
	handle uint64
}

// ReadAt satisfies io.ReaderAt, including its two fussy rules: fill p completely
// unless something stopped it, and report a short read with a non-nil error
// rather than as a success.
func (b *brokerReaderAt) ReadAt(p []byte, off int64) (int, error) {
	read := 0
	for read < len(p) {
		want := len(p) - read
		if want > chunkSize {
			want = chunkSize
		}
		resp, err := b.client.ReadAt(b.ctx, &pb.ReadAtRequest{
			Off:    off + int64(read),
			Len:    int32(want),
			Handle: b.handle,
		})
		if err != nil {
			return read, decodeError(err)
		}
		read += copy(p[read:], resp.GetData())
		if resp.GetEof() {
			return read, io.EOF
		}
		if len(resp.GetData()) == 0 {
			// Not EOF and no progress: the host is not going to produce the rest, and
			// looping would spin forever. ErrUnexpectedEOF says what happened without
			// claiming the file simply ended.
			return read, io.ErrUnexpectedEOF
		}
	}
	return read, nil
}
