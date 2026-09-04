package testenv

import "testing"

func TestRequired(t *testing.T) {
	cases := []struct {
		env      string
		facility string
		want     bool
	}{
		{"", FUSE, false},
		{"fuse", FUSE, true},
		{"fuse", Xattr, false},
		{"all", Xattr, true},
		{" xattr , fuse ", FUSE, true},
		{"XATTR", Xattr, true},
		{",,", FUSE, false},
		{"other", FUSE, false},
	}
	for _, c := range cases {
		t.Setenv(EnvVar, c.env)
		if got := Required(c.facility); got != c.want {
			t.Errorf("%s=%q Required(%q) = %v, want %v", EnvVar, c.env, c.facility, got, c.want)
		}
	}
}

// fakeTB records which branch Unavailable took.
type fakeTB struct{ fataled, skipped bool }

func (f *fakeTB) Helper()               {}
func (f *fakeTB) Fatalf(string, ...any) { f.fataled = true }
func (f *fakeTB) Skipf(string, ...any)  { f.skipped = true }

func TestUnavailable(t *testing.T) {
	t.Run("skips when not required", func(t *testing.T) {
		t.Setenv(EnvVar, "")
		var tb fakeTB
		Unavailable(&tb, FUSE, "no /dev/fuse")
		if tb.fataled || !tb.skipped {
			t.Fatalf("got fataled=%v skipped=%v, want skip", tb.fataled, tb.skipped)
		}
	})
	t.Run("fails when required", func(t *testing.T) {
		t.Setenv(EnvVar, "fuse")
		var tb fakeTB
		Unavailable(&tb, FUSE, "no /dev/fuse")
		if !tb.fataled || tb.skipped {
			t.Fatalf("got fataled=%v skipped=%v, want fatal", tb.fataled, tb.skipped)
		}
	})
	t.Run("other facility still skips", func(t *testing.T) {
		t.Setenv(EnvVar, "xattr")
		var tb fakeTB
		Unavailable(&tb, FUSE, "no /dev/fuse")
		if tb.fataled || !tb.skipped {
			t.Fatalf("got fataled=%v skipped=%v, want skip", tb.fataled, tb.skipped)
		}
	})
}
