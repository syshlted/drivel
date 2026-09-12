package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"

	goplugin "github.com/hashicorp/go-plugin"

	"github.com/zishmusic/drivel/plugin/internal/pb"
	"github.com/zishmusic/drivel/provider"
)

// chunkSize is how much content one stream message carries.
//
// 256 KiB is chosen against the two things that actually vary: the per-message
// overhead of a gRPC frame, which stops mattering well below this, and the
// buffer a sender and a receiver each hold, which starts mattering well above
// it. It is deliberately unrelated to ranges.DefaultBlockSize — that number is
// the granularity at which drivel decides what to *send*, and this one is how
// the bytes get there. Coupling them would make a change to either read as a
// change to both.
const chunkSize = 256 << 10

// server is the plugin-process half of the seam: it serves the Provider service
// on top of one real provider.Store.
//
// Its shape follows one rule: **it adds no behaviour.** Every method is a
// translation and a call. Anything clever here would be behaviour that a
// backend gets when it is loaded as a plugin and does not get when it is
// constructed directly, which would make the two ways of running the same code
// disagree — and in-process construction is what every one of that backend's
// tests uses.
type server struct {
	pb.UnimplementedProviderServer

	factory provider.Factory
	lg      *log.Logger
	broker  *goplugin.GRPCBroker

	mu      sync.Mutex
	store   provider.Store
	caps    provider.CapabilitySet
	open    bool
	content *contentDialer
}

// Open builds the backend from the configuration the host sent and answers with
// what it can do.
//
// The capability answer is computed from the real store by provider.Capabilities
// — the same function an in-process host would call — so a backend declares its
// capabilities in exactly one way whichever side of the seam it is on.
func (s *server) Open(ctx context.Context, req *pb.OpenRequest) (*pb.OpenResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open {
		// One process serves one mount, so a second Open is not a re-configuration
		// — it is a host bug or something else talking to this socket. Either way
		// the honest answer is no.
		return nil, encodeError(errors.New("plugin: already open"))
	}
	store, err := s.factory(ctx, provider.Params{
		Config: provider.Config(req.GetConfig()),
		Log:    s.lg,
	})
	if err != nil {
		return nil, encodeError(err)
	}
	if store == nil {
		return nil, encodeError(errors.New("plugin: factory returned a nil Store with no error"))
	}
	s.store = store
	s.caps = provider.Capabilities(store)
	s.content = &contentDialer{broker: s.broker, brokerID: req.GetContentBrokerId()}
	s.open = true
	return &pb.OpenResponse{Capabilities: s.caps.Names()}, nil
}

// Close releases the backend if it announced a way to be released.
//
// It exists so that a backend holding a local database — Drive's path index is
// the one in the tree — commits and unlocks rather than being terminated
// mid-write. The host calls it before killing the process, and ignores a
// failure: by then there is nothing useful left to do about one.
func (s *server) Close(context.Context, *pb.CloseRequest) (*pb.CloseResponse, error) {
	s.mu.Lock()
	store, content := s.store, s.content
	s.store, s.content = nil, nil
	s.mu.Unlock()

	if content != nil {
		content.close()
	}

	if c, ok := store.(io.Closer); ok {
		if err := c.Close(); err != nil {
			return nil, encodeError(err)
		}
	}
	return &pb.CloseResponse{}, nil
}

// current returns the opened store, or the error a call arriving before Open
// deserves.
func (s *server) current() (provider.Store, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store == nil {
		return nil, encodeError(errors.New("plugin: not open"))
	}
	return s.store, nil
}

// ---------------------------------------------------------------- Store

func (s *server) Put(stream pb.Provider_PutServer) error {
	store, err := s.current()
	if err != nil {
		return err
	}
	// The first message names the path and the rest are content, so the reader
	// handed to the backend starts only once the path is known. chunkReader does
	// the rest: it turns the remaining messages into an io.Reader without
	// buffering the whole file, which is the entire reason Put streams.
	first, err := stream.Recv()
	if err != nil {
		return encodeError(fmt.Errorf("put: reading the path: %w", err))
	}
	path := first.GetPath()
	if path == "" && first.GetData() != nil {
		return encodeError(errors.New("put: content arrived before a path"))
	}

	r := &chunkReader{recv: func() ([]byte, error) {
		msg, err := stream.Recv()
		if err != nil {
			return nil, err
		}
		return msg.GetData(), nil
	}}
	rf, err := store.Put(stream.Context(), path, r)
	if err != nil {
		return encodeError(err)
	}
	// Drain anything the host is still sending. Returning while the client is
	// mid-send makes gRPC cancel the stream, and the host would see its own
	// successful upload as a transport failure.
	r.drain()
	return stream.SendAndClose(&pb.FileResponse{File: fileToPB(rf)})
}

func (s *server) Mkdir(ctx context.Context, req *pb.MkdirRequest) (*pb.FileResponse, error) {
	store, err := s.current()
	if err != nil {
		return nil, err
	}
	rf, err := store.Mkdir(ctx, req.GetPath())
	if err != nil {
		return nil, encodeError(err)
	}
	return &pb.FileResponse{File: fileToPB(rf)}, nil
}

func (s *server) Move(ctx context.Context, req *pb.MoveRequest) (*pb.FileResponse, error) {
	store, err := s.current()
	if err != nil {
		return nil, err
	}
	rf, err := store.Move(ctx, req.GetOldPath(), req.GetNewPath())
	if err != nil {
		return nil, encodeError(err)
	}
	return &pb.FileResponse{File: fileToPB(rf)}, nil
}

func (s *server) Remove(ctx context.Context, req *pb.RemoveRequest) (*pb.RemoveResponse, error) {
	store, err := s.current()
	if err != nil {
		return nil, err
	}
	if err := store.Remove(ctx, req.GetPath()); err != nil {
		return nil, encodeError(err)
	}
	return &pb.RemoveResponse{}, nil
}

func (s *server) Get(req *pb.GetRequest, stream pb.Provider_GetServer) error {
	store, err := s.current()
	if err != nil {
		return err
	}
	rc, err := store.Get(stream.Context(), req.GetPath())
	if err != nil {
		return encodeError(err)
	}
	return sendChunks(rc, stream.Send)
}

func (s *server) Stat(ctx context.Context, req *pb.StatRequest) (*pb.StatResponse, error) {
	store, err := s.current()
	if err != nil {
		return nil, err
	}
	rf, ok, err := store.Stat(ctx, req.GetPath())
	if err != nil {
		return nil, encodeError(err)
	}
	// Absence travels in the response, not as codes.NotFound. Stat is the one
	// call in the seam where "there is no such object" is an ordinary answer, and
	// a status code would put it in the same channel as failures that a caller
	// retries.
	if !ok {
		return &pb.StatResponse{Ok: false}, nil
	}
	return &pb.StatResponse{File: fileToPB(rf), Ok: true}, nil
}

// ---------------------------------------------------- optional capabilities
//
// Each of these is served whether or not the backend implements it. The stub
// that answers when it does not is not dead code: a host that has negotiated
// capabilities will never call it, but a host that has a bug, or a third-party
// tool talking to this socket, will get an error naming the missing capability
// instead of a nil dereference.

func (s *server) StartCursor(ctx context.Context, _ *pb.StartCursorRequest) (*pb.StartCursorResponse, error) {
	store, err := s.current()
	if err != nil {
		return nil, err
	}
	cs, ok := provider.AsChangeSource(store)
	if !ok {
		return nil, unsupported(provider.CapChangeSource)
	}
	cur, err := cs.StartCursor(ctx)
	if err != nil {
		return nil, encodeError(err)
	}
	return &pb.StartCursorResponse{Cursor: cur}, nil
}

func (s *server) Changes(ctx context.Context, req *pb.ChangesRequest) (*pb.ChangesResponse, error) {
	store, err := s.current()
	if err != nil {
		return nil, err
	}
	cs, ok := provider.AsChangeSource(store)
	if !ok {
		return nil, unsupported(provider.CapChangeSource)
	}
	changes, next, err := cs.Changes(ctx, req.GetCursor())
	if err != nil {
		return nil, encodeError(err)
	}
	out := make([]*pb.Change, len(changes))
	for i, c := range changes {
		out[i] = changeToPB(c)
	}
	return &pb.ChangesResponse{Changes: out, Next: next}, nil
}

func (s *server) Enumerate(ctx context.Context, req *pb.EnumerateRequest) (*pb.EnumerateResponse, error) {
	store, err := s.current()
	if err != nil {
		return nil, err
	}
	e, ok := provider.AsEnumerator(store)
	if !ok {
		return nil, unsupported(provider.CapEnumerator)
	}
	files, next, err := e.Enumerate(ctx, req.GetCursor())
	if err != nil {
		return nil, encodeError(err)
	}
	out := make([]*pb.File, len(files))
	for i, f := range files {
		out[i] = fileToPB(f)
	}
	return &pb.EnumerateResponse{Files: out, Next: next}, nil
}

func (s *server) GetRange(req *pb.GetRangeRequest, stream pb.Provider_GetRangeServer) error {
	store, err := s.current()
	if err != nil {
		return err
	}
	rg, ok := provider.AsRangeGetter(store)
	if !ok {
		return unsupported(provider.CapRangeGetter)
	}
	rc, err := rg.GetRange(stream.Context(), req.GetPath(), req.GetOff(), req.GetLength())
	if err != nil {
		return encodeError(err)
	}
	return sendChunks(rc, stream.Send)
}

func (s *server) PutRange(ctx context.Context, req *pb.PutRangeRequest) (*pb.FileResponse, error) {
	store, err := s.current()
	if err != nil {
		return nil, err
	}
	rp, ok := provider.AsRangePutter(store)
	if !ok {
		return nil, unsupported(provider.CapRangePutter)
	}

	// The local file is read back through the host's content service rather than
	// streamed into this call, so the backend keeps the random access
	// provider.RangePutter promises it. See content.go for why.
	s.mu.Lock()
	dialer := s.content
	s.mu.Unlock()
	if dialer == nil {
		return nil, encodeError(errors.New("put-range: not open"))
	}
	client, err := dialer.dial()
	if err != nil {
		return nil, encodeError(err)
	}

	src := &brokerReaderAt{client: client, ctx: ctx, handle: req.GetSourceHandle()}
	rf, err := rp.PutRange(ctx, req.GetPath(), src, req.GetSize(), extentsFromPB(req.GetExtents()))
	if err != nil {
		return nil, encodeError(err)
	}
	return &pb.FileResponse{File: fileToPB(rf)}, nil
}

func (s *server) HashContent(stream pb.Provider_HashContentServer) error {
	store, err := s.current()
	if err != nil {
		return err
	}
	h, ok := provider.AsContentHasher(store)
	if !ok {
		return unsupported(provider.CapContentHasher)
	}
	r := &chunkReader{recv: func() ([]byte, error) {
		c, err := stream.Recv()
		if err != nil {
			return nil, err
		}
		return c.GetData(), nil
	}}
	sum, err := h.HashContent(r)
	if err != nil {
		return encodeError(err)
	}
	r.drain()
	return stream.SendAndClose(&pb.HashContentResponse{Hash: sum})
}

// unsupported is the answer to a call for a capability this backend does not
// have. It is never retryable: no amount of waiting grows a method.
func unsupported(c provider.Capability) error {
	return encodeError(fmt.Errorf("plugin: this backend does not implement %s", c))
}

// sendChunks copies a reader into a stream and closes it.
func sendChunks(rc io.ReadCloser, send func(*pb.Chunk) error) error {
	defer func() { _ = rc.Close() }()
	buf := make([]byte, chunkSize)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			if serr := send(&pb.Chunk{Data: buf[:n]}); serr != nil {
				return serr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return encodeError(err)
		}
	}
}
