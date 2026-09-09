package completion

import (
	"os"
	"strconv"
)

func itoa(n int) string { return strconv.Itoa(n) }

func writeFile(path string, b []byte) error { return os.WriteFile(path, b, 0o600) }
