package fuse

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
)

// generic/300 regression: allocator errors travel wrapped in fmt.Errorf("%w")
// chains (e.g. "alloc leaf 145: no space left on device").  The bare type
// switch mislabeled them EIO — fio tolerates ENOSPC via ignore_error but
// aborted job dispatch on the EIO.
func TestErrToErrnoUnwrapsWrappedErrno(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want syscall.Errno
	}{
		{"nil", nil, 0},
		{"bare errno", syscall.ENOSPC, syscall.ENOSPC},
		{"single wrap", fmt.Errorf("alloc leaf %d: %w", 145, syscall.ENOSPC), syscall.ENOSPC},
		{"deep wrap", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", syscall.EINVAL)), syscall.EINVAL},
		{"no errno inside", errors.New("btree parse failure"), syscall.EIO},
	}
	for _, c := range cases {
		if got := errToErrno(c.err); got != c.want {
			t.Errorf("%s: errToErrno(%v) = %d, want %d", c.name, c.err, got, c.want)
		}
	}
}