package gdrive

import (
	"context"
	"os"
	"testing"
	"time"
)

// A live pre-flight against a real account, off unless DRIVEL_LIVE_RIG is set.
// It answers the two §2.1 questions no fake can: how much Drive quota is left
// (MC-10 parks 6 GiB of a free account's 15 GB), and which folder ID the rig
// should be pinned to — because "-drive-root root" plus a reconcile is the
// documented catastrophic configuration.
func TestLiveRigPreflight(t *testing.T) {
	if os.Getenv("DRIVEL_LIVE_RIG") == "" {
		t.Skip("set DRIVEL_LIVE_RIG=1 with -creds/-token env to run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	d, err := Open(ctx, Config{
		Credentials: os.Getenv("DRIVEL_CREDS"),
		Token:       os.Getenv("DRIVEL_TOKEN"),
		RootID:      "root",
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = d.Close() }()

	about, err := d.svc.About.Get().Fields("user(emailAddress,displayName),storageQuota").Context(ctx).Do()
	if err != nil {
		t.Fatalf("about: %v", err)
	}
	q := about.StorageQuota
	gib := func(n int64) float64 { return float64(n) / (1 << 30) }
	t.Logf("account: %s <%s>", about.User.DisplayName, about.User.EmailAddress)
	if q.Limit > 0 {
		t.Logf("quota:   %.2f GiB used of %.2f GiB (%.2f GiB free); drive=%.2f trash=%.2f",
			gib(q.Usage), gib(q.Limit), gib(q.Limit-q.Usage), gib(q.UsageInDrive), gib(q.UsageInDriveTrash))
	} else {
		t.Logf("quota:   unlimited (usage %.2f GiB)", gib(q.Usage))
	}

	// What is already in this Drive: the check that says "throwaway" is true.
	res, err := d.svc.Files.List().
		Q("trashed = false and 'root' in parents").
		Fields("files(id,name,mimeType,size)").PageSize(50).Context(ctx).Do()
	if err != nil {
		t.Fatalf("list root: %v", err)
	}
	t.Logf("top level: %d object(s)", len(res.Files))
	for _, f := range res.Files {
		kind := "file"
		if f.MimeType == folderMIME {
			kind = "FOLDER"
		}
		t.Logf("  %-8s %-40s %s", kind, f.Name, f.Id)
	}

	// The rig folder's ID, which is what every client must be pinned to: mapping a
	// mount to My Drive with "-drive-root root" and then reconciling is the
	// documented catastrophic configuration (§2.1). Reported, never created —
	// this probe only reads, so running it can never change the account it is
	// describing.
	const rigName = "drivel-rig"
	for _, f := range res.Files {
		if f.Name == rigName && f.MimeType == folderMIME {
			t.Logf("RIG FOLDER ID: %s", f.Id)
			return
		}
	}
	t.Logf("no %q folder at the top level; create one and pin the rig to its ID", rigName)
}
