package plugin

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"io"
	"sync"

	"github.com/zishmusic/drivel/plugin/internal/pb"
	"github.com/zishmusic/drivel/provider"
	"github.com/zishmusic/drivel/ranges"
)

// session hands out a live connection to the backend process, launching or
// relaunching it as needed. It is an interface so that the proxy below can be
// tested against a plugin that is not a subprocess at all.
type session interface {
	// acquire returns a live stub together with the content registry belonging to
	// the same connection. The two are handed out as a pair so that a handle
	// registered for a call can never be looked up against a process that has
	// been restarted since.
	acquire(ctx context.Context) (pb.ProviderClient, *contentRegistry, error)
}

// remoteStore is the host-side provider.Store for a backend in another process.
//
// It implements every optional interface in the seam, because it is one type
// serving every backend — and that is exactly why it also implements
// provider.Declarer. The capability set it declares is the one the backend
// answered with at Open, so provider.Capabilities narrows this type's universal
// method set down to what the backend behind it can actually do. Without that,
// a mount over a backend with no change feed would poll one forever.
type remoteStore struct {
	sess session
	caps provider.CapabilitySet
	// lg reports on this mount's logger, already carrying the backend's kind. Nil
	// is allowed and silent, because the test doubles have nowhere to report to.
	lg func(format string, args ...any)

	// The digest the backend computes, identified once on first use. See hash.go
	// for why the host works it out rather than being told, and HashContent for
	// what it saves. hashNew stays nil when nothing matched, which is the
	// stream-it-across path every backend used before.
	hashOnce sync.Once
	hashNew  func() hash.Hash
}

func (s *remoteStore) logf(format string, args ...any) {
	if s.lg == nil {
		return
	}
	s.lg(format, args...)
}

var (
	_ provider.Store         = (*remoteStore)(nil)
	_ provider.Declarer      = (*remoteStore)(nil)
	_ provider.ChangeSource  = (*remoteStore)(nil)
	_ provider.Enumerator    = (*remoteStore)(nil)
	_ provider.RangeGetter   = (*remoteStore)(nil)
	_ provider.RangePutter   = (*remoteStore)(nil)
	_ provider.ContentHasher = (*remoteStore)(nil)
)

// Capabilities reports what the backend said it could do at Open.
//
// It is fixed for the life of the mount, deliberately. The engine wires itself
// to the answer once — whether there is a pull loop at all is decided at mount
// time — so a set that changed under it would leave a downloader polling a feed
// that is no longer there. A restarted backend that reports something different
// is logged about loudly and held to the original answer; see host.go.
func (s *remoteStore) Capabilities() provider.CapabilitySet { return s.caps }

// ---------------------------------------------------------------- Store

func (s *remoteStore) Put(ctx context.Context, path string, r io.Reader) (provider.RemoteFile, error) {
	api, _, err := s.sess.acquire(ctx)
	if err != nil {
		return provider.RemoteFile{}, err
	}
	// The stream gets a context this call owns, so that a local read failure can
	// abort it rather than end it cleanly. See sendError.
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := api.Put(sctx)
	if err != nil {
		return provider.RemoteFile{}, decodeError(err)
	}
	// The path goes first and alone, so the backend can open its destination
	// before a byte of content arrives.
	if err := stream.Send(&pb.PutRequest{Msg: &pb.PutRequest_Path{Path: path}}); err != nil {
		return provider.RemoteFile{}, decodeError(err)
	}
	if err := chunked(r, func(b []byte) error {
		return stream.Send(&pb.PutRequest{Msg: &pb.PutRequest_Data{Data: b}})
	}); err != nil {
		var se sendError
		if !errors.As(err, &se) {
			// The local file could not be read. Cancel rather than close: closing
			// cleanly would tell the backend the content ended here and it would
			// store a truncated object and report success.
			cancel()
			return provider.RemoteFile{}, fmt.Errorf("reading %s to upload: %w", path, err)
		}
		// The wire failed, which usually means the backend already gave up and
		// closed the stream. Its reason is the useful error; the send error is the
		// symptom.
		if _, rerr := stream.CloseAndRecv(); rerr != nil {
			return provider.RemoteFile{}, decodeError(rerr)
		}
		return provider.RemoteFile{}, decodeError(se.err)
	}
	resp, err := stream.CloseAndRecv()
	if err != nil {
		return provider.RemoteFile{}, decodeError(err)
	}
	return fileFromPB(resp.GetFile()), nil
}

func (s *remoteStore) Mkdir(ctx context.Context, path string) (provider.RemoteFile, error) {
	api, _, err := s.sess.acquire(ctx)
	if err != nil {
		return provider.RemoteFile{}, err
	}
	resp, err := api.Mkdir(ctx, &pb.MkdirRequest{Path: path})
	if err != nil {
		return provider.RemoteFile{}, decodeError(err)
	}
	return fileFromPB(resp.GetFile()), nil
}

func (s *remoteStore) Move(ctx context.Context, oldPath, newPath string) (provider.RemoteFile, error) {
	api, _, err := s.sess.acquire(ctx)
	if err != nil {
		return provider.RemoteFile{}, err
	}
	resp, err := api.Move(ctx, &pb.MoveRequest{OldPath: oldPath, NewPath: newPath})
	if err != nil {
		return provider.RemoteFile{}, decodeError(err)
	}
	return fileFromPB(resp.GetFile()), nil
}

func (s *remoteStore) Remove(ctx context.Context, path string) error {
	api, _, err := s.sess.acquire(ctx)
	if err != nil {
		return err
	}
	if _, err := api.Remove(ctx, &pb.RemoveRequest{Path: path}); err != nil {
		return decodeError(err)
	}
	return nil
}

func (s *remoteStore) Get(ctx context.Context, path string) (io.ReadCloser, error) {
	api, _, err := s.sess.acquire(ctx)
	if err != nil {
		return nil, err
	}
	// The returned reader outlives this call, so the stream gets a context of its
	// own that Close cancels. Without it, a caller that stops reading half way
	// through a large file leaves the backend sending into a stream nobody drains
	// until the mount shuts down.
	sctx, cancel := context.WithCancel(ctx)
	stream, err := api.Get(sctx, &pb.GetRequest{Path: path})
	if err != nil {
		cancel()
		return nil, decodeError(err)
	}
	return newStreamReadCloser(stream.Recv, cancel), nil
}

func (s *remoteStore) Stat(ctx context.Context, path string) (provider.RemoteFile, bool, error) {
	api, _, err := s.sess.acquire(ctx)
	if err != nil {
		return provider.RemoteFile{}, false, err
	}
	resp, err := api.Stat(ctx, &pb.StatRequest{Path: path})
	if err != nil {
		return provider.RemoteFile{}, false, decodeError(err)
	}
	if !resp.GetOk() {
		return provider.RemoteFile{}, false, nil
	}
	return fileFromPB(resp.GetFile()), true, nil
}

// ---------------------------------------------------- optional capabilities

func (s *remoteStore) StartCursor(ctx context.Context) (string, error) {
	api, _, err := s.sess.acquire(ctx)
	if err != nil {
		return "", err
	}
	resp, err := api.StartCursor(ctx, &pb.StartCursorRequest{})
	if err != nil {
		return "", decodeError(err)
	}
	return resp.GetCursor(), nil
}

func (s *remoteStore) Changes(ctx context.Context, cursor string) ([]provider.RemoteChange, string, error) {
	api, _, err := s.sess.acquire(ctx)
	if err != nil {
		return nil, "", err
	}
	resp, err := api.Changes(ctx, &pb.ChangesRequest{Cursor: cursor})
	if err != nil {
		return nil, "", decodeError(err)
	}
	out := make([]provider.RemoteChange, len(resp.GetChanges()))
	for i, c := range resp.GetChanges() {
		out[i] = changeFromPB(c)
	}
	return out, resp.GetNext(), nil
}

func (s *remoteStore) Enumerate(ctx context.Context, cursor string) ([]provider.RemoteFile, string, error) {
	api, _, err := s.sess.acquire(ctx)
	if err != nil {
		return nil, "", err
	}
	resp, err := api.Enumerate(ctx, &pb.EnumerateRequest{Cursor: cursor})
	if err != nil {
		return nil, "", decodeError(err)
	}
	out := make([]provider.RemoteFile, len(resp.GetFiles()))
	for i, f := range resp.GetFiles() {
		out[i] = fileFromPB(f)
	}
	return out, resp.GetNext(), nil
}

func (s *remoteStore) GetRange(ctx context.Context, path string, off, length int64) (io.ReadCloser, error) {
	api, _, err := s.sess.acquire(ctx)
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithCancel(ctx)
	stream, err := api.GetRange(sctx, &pb.GetRangeRequest{Path: path, Off: off, Length: length})
	if err != nil {
		cancel()
		return nil, decodeError(err)
	}
	return newStreamReadCloser(stream.Recv, cancel), nil
}

func (s *remoteStore) PutRange(ctx context.Context, path string, src io.ReaderAt, size int64, extents []ranges.Range) (provider.RemoteFile, error) {
	api, reg, err := s.sess.acquire(ctx)
	if err != nil {
		return provider.RemoteFile{}, err
	}

	// Make the local file readable for the duration of this one call, and not a
	// moment longer. The handle is the plugin's only way to reach it.
	handle, done := reg.add(src, size)
	defer done()

	resp, err := api.PutRange(ctx, &pb.PutRangeRequest{
		Path:         path,
		Size:         size,
		Extents:      extentsToPB(extents),
		SourceHandle: handle,
	})
	if err != nil {
		return provider.RemoteFile{}, decodeError(err)
	}
	return fileFromPB(resp.GetFile()), nil
}

// HashContent returns the backend's digest of r, computing it here when the host
// can reproduce the backend's algorithm and streaming the content over only when
// it cannot.
//
// The saving is the point. M6's gate 3 runs before most content pushes, so
// streaming meant every candidate file crossed the socket to decide whether it
// should cross the network — and a file that then needed pushing crossed twice.
// Drive declines RangePutter, so gate 2 always falls through to gate 3 and that
// was every content push at or above one block.
//
// The algorithm is still entirely the provider's. Nothing here assumes one: the
// host identifies it by asking the backend to digest bytes it can check (see
// matchLocalHash), and a digest this build cannot reproduce keeps the old path
// exactly. A backend loaded in process is untouched — it never reaches this type.
func (s *remoteStore) HashContent(r io.Reader) (string, error) {
	s.hashOnce.Do(func() { s.identifyHash(s.hashRemote) })
	if s.hashNew != nil {
		sum, err := localDigest(s.hashNew, r)
		if err != nil {
			// Same wording the streaming path uses for the same failure, because it
			// is the same failure: the local file could not be read. A digest over a
			// short read would be a digest of content nobody holds.
			return "", fmt.Errorf("reading content to hash: %w", err)
		}
		return sum, nil
	}
	return s.hashRemote(r)
}

// identifyHash runs the probe, once per mount, on the first hash a gate asks for.
//
// ask is a parameter rather than s.hashRemote directly so that the decision can
// be tested without a backend behind it — the whole point of this function is
// which of two paths every later call takes.
//
// It cannot fail the caller: every unhappy outcome — a probe the backend refused,
// a digest this build has no implementation of — leaves hashNew nil and the
// content streaming, which is what every backend did before this existed.
func (s *remoteStore) identifyHash(ask func(io.Reader) (string, error)) {
	name, newHash, err := matchLocalHash(ask)
	switch {
	case err != nil:
		s.logf("content digest: the backend refused to identify it (%v); content will be sent over to be hashed", err)
	case newHash == nil:
		s.logf("content digest: not one this build can compute; content will be sent over to be hashed")
	default:
		s.hashNew = newHash
		s.logf("content digest: %s, computed locally — file content is no longer sent to the backend to be hashed", name)
	}
}

// hashRemote is the original implementation: stream the content to the backend
// and let it answer. It is both the fallback and the probe.
func (s *remoteStore) hashRemote(r io.Reader) (string, error) {
	// ContentHasher takes no context: it is a local computation in the provider's
	// own digest, and in process it never blocks on anything. Out of process it
	// crosses a socket, so it gets a context that is cancelled when this returns
	// — enough to release the stream, and not a behaviour change, since the call
	// is synchronous either way.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	api, _, err := s.sess.acquire(ctx)
	if err != nil {
		return "", err
	}
	stream, err := api.HashContent(ctx)
	if err != nil {
		return "", decodeError(err)
	}
	if err := chunked(r, func(b []byte) error {
		return stream.Send(&pb.Chunk{Data: b})
	}); err != nil {
		var se sendError
		if !errors.As(err, &se) {
			// Same rule as Put's, and it matters as much: a digest computed over a
			// truncated read would not match the remote, so M6's unchanged-content
			// gate would decline — safe — but a digest that happened to match would
			// skip a push the file needed. Cancel and report the read failure.
			cancel()
			return "", fmt.Errorf("reading content to hash: %w", err)
		}
		if _, rerr := stream.CloseAndRecv(); rerr != nil {
			return "", decodeError(rerr)
		}
		return "", decodeError(se.err)
	}
	resp, err := stream.CloseAndRecv()
	if err != nil {
		return "", decodeError(err)
	}
	return resp.GetHash(), nil
}

// streamReadCloser adapts a server stream of content messages to the
// io.ReadCloser the seam hands back from Get and GetRange.
type streamReadCloser struct {
	*chunkReader
	cancel context.CancelFunc
}

func newStreamReadCloser(recv func() (*pb.Chunk, error), cancel context.CancelFunc) *streamReadCloser {
	return &streamReadCloser{
		chunkReader: &chunkReader{recv: func() ([]byte, error) {
			c, err := recv()
			if err != nil {
				return nil, err
			}
			return c.GetData(), nil
		}},
		cancel: cancel,
	}
}

// Close ends the stream. It is safe to call more than once and never reports an
// error: there is nothing a caller could do about a stream it has finished with,
// and M5's hydrator closes readers on paths where an error would be reported as
// a failed hydration — which is EIO to the user, over nothing.
func (s *streamReadCloser) Close() error {
	s.cancel()
	return nil
}

// describeMismatch renders a capability change across a restart, for the log
// line that reports one.
func describeMismatch(kind string, was, now provider.CapabilitySet) string {
	return fmt.Sprintf("plugin %s restarted reporting different capabilities (was: %s; now: %s); "+
		"keeping the original, since this mount is already wired to it — restart drivel to pick up the change",
		kind, was, now)
}
