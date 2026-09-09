package gdrive

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	drive "google.golang.org/api/drive/v3"
)

// The one claim about trashing that no fake can settle: that real Drive accepts
// files.update{trashed:true}, and that what it produces is a path drivel can no
// longer see and an object a user still can.
//
// Off unless DRIVEL_LIVE_RIG is set, and it writes — unlike the pre-flight above,
// which only reads. It therefore insists on a folder ID of its own and refuses
// "root": a write test against the whole of My Drive is the catastrophic
// configuration §2.1 exists to keep away from. Everything it creates it removes,
// including emptying the trashed copy afterwards, so a run leaves the rig as it
// found it.
func TestLiveRemoveTrashes(t *testing.T) {
	if os.Getenv("DRIVEL_LIVE_RIG") == "" {
		t.Skip("set DRIVEL_LIVE_RIG=1 with -creds/-token/-root env to run")
	}
	root := os.Getenv("DRIVEL_LIVE_ROOT")
	if root == "" || root == "root" {
		t.Fatal("DRIVEL_LIVE_ROOT must name a dedicated folder ID, never \"root\"")
	}
	lctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	cfg := Config{
		Credentials: os.Getenv("DRIVEL_CREDS"),
		Token:       os.Getenv("DRIVEL_TOKEN"),
		RootID:      root,
	}
	d, err := Open(lctx, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Registered before anything it has to outlive: t.Cleanup runs LIFO and runs
	// *after* every deferred call, so `defer d.Close()` shuts the HTTP/3 transport
	// down before the cleanups that still need it ("http3: transport is closed",
	// and two objects left in the rig).
	t.Cleanup(func() { _ = d.Close() })

	// A name nothing else in the rig uses, and one that says what left it behind
	// if an assertion fails before the cleanup runs.
	const name = "drivel-live-trash-probe.txt"
	rf, err := d.Put(lctx, name, strings.NewReader("probe"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	id, ok := resolve(t, d, name)
	if !ok {
		t.Fatalf("just-written %s does not resolve", name)
	}
	t.Cleanup(func() {
		// Permanent, deliberately: the point of the test is what the trash holds
		// during it, not what it holds afterwards.
		ctx, c := context.WithTimeout(context.Background(), 30*time.Second)
		defer c()
		if err := d.svc.Files.Delete(id).Context(ctx).Do(); err != nil {
			t.Logf("cleanup: %s (%s) left behind: %v", name, id, err)
		}
	})
	t.Logf("wrote %s as %s (version %s)", name, id, rf.Version)

	if err := d.Remove(lctx, name); err != nil {
		t.Fatalf("remove: %v", err)
	}

	// Half one: the object is in the trash, not gone. Asked of Drive directly,
	// because every path drivel resolves through is supposed to hide it.
	got, err := d.svc.Files.Get(id).Fields("id,name,trashed,explicitlyTrashed").Context(lctx).Do()
	if err != nil {
		t.Fatalf("the removal deleted %s outright rather than trashing it: %v", id, err)
	}
	if !got.Trashed {
		t.Fatalf("%s survived the removal untrashed", id)
	}
	if !got.ExplicitlyTrashed {
		t.Errorf("%s is trashed but not explicitly; it should be restorable on its own", id)
	}

	// Half two: a folder takes its subtree with it. Drive's `trashed` is
	// documented as "explicitly, or from a trashed parent folder", and the docs
	// tell users a deleted directory carries its contents — but below this test
	// that claim rests on the fake, which models the behaviour rather than
	// proving it.
	const dir, child = "drivel-live-trash-dir", "drivel-live-trash-dir/inside.txt"
	if _, err := d.Put(lctx, child, strings.NewReader("inside")); err != nil {
		t.Fatalf("put %s: %v", child, err)
	}
	dirID, ok := resolve(t, d, dir)
	if !ok {
		t.Fatalf("%s does not resolve after writing a file into it", dir)
	}
	childID, ok := resolve(t, d, child)
	if !ok {
		t.Fatalf("%s does not resolve", child)
	}
	t.Cleanup(func() {
		ctx, c := context.WithTimeout(context.Background(), 30*time.Second)
		defer c()
		if err := d.svc.Files.Delete(dirID).Context(ctx).Do(); err != nil {
			t.Logf("cleanup: %s (%s) left behind: %v", dir, dirID, err)
		}
	})
	if err := d.Remove(lctx, dir); err != nil {
		t.Fatalf("remove %s: %v", dir, err)
	}
	// ...but not at once. Measured against the rig on 2026-09-09: the child still
	// read trashed=false immediately after the parent was trashed, and read
	// trashed=true (explicitlyTrashed=false, i.e. inherited) a minute later. So
	// this polls, and what it asserts is that the inheritance arrives at all.
	//
	// The lag costs drivel nothing, and it is worth being explicit about why. A
	// user's rm -rf unlinks the children first, so each is trashed in its own
	// right and none of this is on the path; and during the window, a flat sweep
	// that still listed a child would park it on a parent the listing no longer
	// contains and drop it as outside the mount, while a scoped descent never
	// reaches it because the trashed folder is not among its parent's children.
	deadline := time.Now().Add(90 * time.Second)
	var inside *drive.File
	for {
		inside, err = d.svc.Files.Get(childID).Fields("id,trashed,explicitlyTrashed").Context(lctx).Do()
		if err != nil {
			t.Fatalf("child of a trashed folder: %v", err)
		}
		if inside.Trashed || time.Now().After(deadline) {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if !inside.Trashed {
		t.Errorf("%s was still not trashed 90s after its parent was; a folder does not carry its subtree", child)
	} else if inside.ExplicitlyTrashed {
		t.Errorf("%s was trashed in its own right rather than by inheritance; restoring the folder would not restore it", child)
	}

	// Half three: to drivel the path is gone. A second client is what makes this a
	// real check — the first one forgot the path locally when it removed it.
	peer, err := Open(lctx, cfg)
	if err != nil {
		t.Fatalf("open peer: %v", err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	if _, ok, err := peer.Stat(lctx, name); err != nil {
		t.Fatalf("peer stat: %v", err)
	} else if ok {
		t.Fatalf("a trashed path still resolves for a fresh client; the removal is invisible as one")
	}
}
