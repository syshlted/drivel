//go:build !linux

package hydrate

import "syscall"

// xattrSupported is false off Linux: the placeholder marker degrades to the
// state-store cache alone. See Hydrator.IsPlaceholder for what that costs.
const xattrSupported = false

func getxattr(string, string) ([]byte, error) { return nil, syscall.ENOTSUP }
func setxattr(string, string, []byte) error   { return syscall.ENOTSUP }
func removexattr(string, string) error        { return syscall.ENOTSUP }

func isNoAttr(err error) bool    { return false }
func isNoSupport(err error) bool { return err == syscall.ENOTSUP }
