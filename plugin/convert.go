package plugin

import (
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/zishmusic/drivel/plugin/internal/pb"
	"github.com/zishmusic/drivel/provider"
	"github.com/zishmusic/drivel/ranges"
)

// The translations between the Go seam and the wire. They are boring on purpose:
// every field maps to one field, and nothing here interprets a value. The one
// judgement call in the file is how a zero time.Time travels, and it is made
// once, below.

// fileToPB renders a provider.RemoteFile for the wire.
func fileToPB(f provider.RemoteFile) *pb.File {
	out := &pb.File{
		Path:       f.Path,
		IsDir:      f.IsDir,
		Size:       f.Size,
		Hash:       f.Hash,
		Version:    f.Version,
		ExportOnly: f.ExportOnly,
	}
	// A zero time.Time is "this backend did not report a modification time", and
	// it has to stay distinguishable from a real timestamp. An unset message says
	// that exactly; a Unix epoch field could not, because 1970 is a time a file
	// can genuinely carry and the sweep compares mtimes.
	if !f.Modified.IsZero() {
		out.Modified = timestamppb.New(f.Modified)
	}
	return out
}

// fileFromPB is fileToPB's inverse. A nil message is the zero RemoteFile, which
// is what a Change with Removed set carries.
func fileFromPB(f *pb.File) provider.RemoteFile {
	if f == nil {
		return provider.RemoteFile{}
	}
	out := provider.RemoteFile{
		Path:       f.GetPath(),
		IsDir:      f.GetIsDir(),
		Size:       f.GetSize(),
		Hash:       f.GetHash(),
		Version:    f.GetVersion(),
		ExportOnly: f.GetExportOnly(),
	}
	if ts := f.GetModified(); ts != nil {
		out.Modified = ts.AsTime()
	}
	return out
}

// changeToPB renders one entry of a change feed.
func changeToPB(c provider.RemoteChange) *pb.Change {
	out := &pb.Change{Path: c.Path, Removed: c.Removed}
	if c.File != nil {
		out.File = fileToPB(*c.File)
	}
	return out
}

// changeFromPB is changeToPB's inverse. The File pointer stays nil when the
// message carries none, because the engine reads nil-ness as "removed" in more
// places than it reads the Removed flag.
func changeFromPB(c *pb.Change) provider.RemoteChange {
	out := provider.RemoteChange{Path: c.GetPath(), Removed: c.GetRemoved()}
	if f := c.GetFile(); f != nil {
		rf := fileFromPB(f)
		out.File = &rf
	}
	return out
}

// extentsToPB renders a dirty-range list for a PutRange call.
func extentsToPB(exts []ranges.Range) []*pb.Extent {
	out := make([]*pb.Extent, len(exts))
	for i, e := range exts {
		out[i] = &pb.Extent{Off: e.Off, Len: e.Len}
	}
	return out
}

// extentsFromPB is extentsToPB's inverse.
func extentsFromPB(exts []*pb.Extent) []ranges.Range {
	out := make([]ranges.Range, len(exts))
	for i, e := range exts {
		out[i] = ranges.Range{Off: e.GetOff(), Len: e.GetLen()}
	}
	return out
}
