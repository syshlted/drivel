package gauth

import (
	"fmt"
	"io"
	"os"
)

// writeSecret writes a credentials or token file, guaranteeing 0600 whether or
// not the file already existed.
//
// os.OpenFile (and os.WriteFile) apply their mode only when they create the
// file. A token.json restored from a backup, copied in by hand, written under a
// permissive umask or left behind by an older build keeps whatever mode it
// arrived with, and drivel would rewrite it in place and leave it that way —
// these two files hold a client secret and a refresh token for the user's entire
// Drive. Narrowing through the descriptor rather than the path means there is no
// window in which the name could point somewhere else.
func writeSecret(path string, write func(io.Writer) error) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("restricting permissions on %s: %w", path, err)
	}
	if err := write(f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close() // a short write surfaces here, not above
}
