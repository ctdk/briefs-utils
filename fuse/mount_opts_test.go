package fuse

import (
	"reflect"
	"testing"
)

// TestMergeMountOptions covers the mount-option merge the daemon applies
// to `mount -t fuse.briefs -o <opts>` requests (generic/128: -o nosuid
// must actually reach the FUSE mount).
func TestMergeMountOptions(t *testing.T) {
	for name, tc := range map[string]struct {
		in   []string
		want []string
	}{
		// No caller options: the kernel-parity defaults alone.
		"nil":    {nil, []string{"suid", "dev", "exec"}},
		"empty":  {[]string{}, []string{"suid", "dev", "exec"}},
		"blanks": {[]string{"", "  "}, []string{"suid", "dev", "exec"}},
		// The three VFS flag pairs: the restrictive word drops its
		// permissive default counterpart.
		"nosuid": {
			[]string{"nosuid"},
			[]string{"dev", "exec", "nosuid"},
		},
		"nodev": {
			[]string{"nodev"},
			[]string{"suid", "exec", "nodev"},
		},
		"noexec": {
			[]string{"noexec"},
			[]string{"suid", "dev", "noexec"},
		},
		// All three restrictive flags plus a kernel-generic extra.
		"nosuid,nodev,noexec,ro": {
			[]string{"nosuid", "nodev", "noexec", "ro"},
			[]string{"nosuid", "nodev", "noexec", "ro"},
		},
		// A permissive request already in the defaults: no duplicate.
		"suid dup": {[]string{"suid"}, []string{"suid", "dev", "exec"}},
		// Unrecognized options pass through verbatim for the kernel
		// to validate (it fails the mount on unknown options).
		"noacl passthrough": {
			[]string{"noacl"},
			[]string{"suid", "dev", "exec", "noacl"},
		},
		"blank entries skipped": {
			[]string{"", "nosuid", " "},
			[]string{"dev", "exec", "nosuid"},
		},
	} {
		got := mergeMountOptions(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: mergeMountOptions(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}

	// The defaults slice must not be mutated by a merge (the package var
	// is shared by every mount).
	if !reflect.DeepEqual(defaultMountOpts, []string{"suid", "dev", "exec"}) {
		t.Fatalf("defaultMountOpts mutated: %q", defaultMountOpts)
	}
}
