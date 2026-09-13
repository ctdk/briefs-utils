// Package fuse: per-request caller status read from /proc/<tid>/status.
//
// The FUSE protocol delivers only the caller's uid/gid/pid in each request
// header. Everything else the kernel would know about current at syscall
// entry — the effective capability set, the umask, the supplementary group
// list — must be read from /proc/<Caller.Pid>/status, which is per-thread:
// Caller.Pid is the issuing thread's tid, and umask has been per-thread
// since 4.7, so this is exactly the value the kernel would have read. The
// caller is blocked inside the syscall while the daemon services the
// request, so the values cannot change underneath the read (except for a
// sibling thread racing a umask change — the same race the kernel itself
// has reading current_umask at entry).
//
// Fallback direction when the status cannot be read (no caller in the
// context, pid 0, /proc unreadable — exited caller, different pid
// namespace): capabilities denied, in-group false (both the safe direction
// for the killpriv decision), umask 0 (no masking; unreachable live, as
// the daemon is root in the caller's pid namespace and the caller is
// blocked).

package fuse

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/hanwen/go-fuse/v2/fuse"
)

const (
	// Capability bits checked by the bridge (include/uapi/linux/capability.h).
	capFSetIDBit   = 7  // CAP_FSETID: setattr_should_drop_suidgid gate
	capSysAdminBit = 21 // CAP_SYS_ADMIN: FITRIM, SETFSLABEL (file.c:258, :304)
)

// callerStatus is the per-request caller state, read once from
// /proc/<pid>/status.
type callerStatus struct {
	capEff uint64
	umask  uint32
	groups []uint32
}

// procCallerStatus is the /proc reader; a package var so tests inject it.
var procCallerStatus = readProcCallerStatus

// readProcCallerStatus parses the CapEff, Umask, and Groups lines of a
// /proc/<pid>/status file. The second return is false when the file cannot
// be read; missing or malformed lines leave that field at its zero value.
func readProcCallerStatus(pid uint32) (callerStatus, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return callerStatus{}, false
	}
	var st callerStatus
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "CapEff:"):
			if f := strings.Fields(line[len("CapEff:"):]); len(f) > 0 {
				if v, err := strconv.ParseUint(f[0], 16, 64); err == nil {
					st.capEff = v
				}
			}
		case strings.HasPrefix(line, "Umask:"):
			if f := strings.Fields(line[len("Umask:"):]); len(f) > 0 {
				if v, err := strconv.ParseUint(f[0], 8, 32); err == nil {
					st.umask = uint32(v)
				}
			}
		case strings.HasPrefix(line, "Groups:"):
			for _, f := range strings.Fields(line[len("Groups:"):]) {
				if v, err := strconv.ParseUint(f, 10, 32); err == nil {
					st.groups = append(st.groups, uint32(v))
				}
			}
		}
	}
	return st, true
}

// loadCallerStatus resolves the request's caller and reads its status.
func loadCallerStatus(ctx context.Context) (callerStatus, bool) {
	caller, ok := fuse.FromContext(ctx)
	if !ok || caller == nil || caller.Pid == 0 {
		return callerStatus{}, false
	}
	return procCallerStatus(caller.Pid)
}

// callerHasCap reports whether the request's caller holds the given
// capability bit. A caller whose status cannot be read is unprivileged.
func callerHasCap(ctx context.Context, bit uint) bool {
	st, ok := loadCallerStatus(ctx)
	if !ok {
		return false
	}
	return st.capEff&(1<<bit) != 0
}

// callerCapSysAdmin reports whether the caller holds CAP_SYS_ADMIN, the
// capability the kernel requires for FITRIM and SETFSLABEL (file.c:258,
// :304). FUSE performs no capability check on the daemon's behalf, so the
// bridge must.
func callerCapSysAdmin(ctx context.Context) bool {
	return callerHasCap(ctx, capSysAdminBit)
}

// callerUmask returns the caller's umask. An unreadable status means no
// masking (umask 0); unreachable live, see the package comment.
func callerUmask(ctx context.Context) uint32 {
	st, ok := loadCallerStatus(ctx)
	if !ok {
		return 0
	}
	return st.umask
}

// callerInGroup reports whether the caller's egid (the FUSE header's Gid)
// or any supplementary group equals gid — in_group_p's check. An
// unreadable status is not-in-group (the safe direction for the sgid
// killpriv decision, which errs toward clearing).
func callerInGroup(ctx context.Context, gid uint32) bool {
	if caller, ok := fuse.FromContext(ctx); ok && caller != nil && caller.Gid == gid {
		return true
	}
	st, ok := loadCallerStatus(ctx)
	if !ok {
		return false
	}
	for _, g := range st.groups {
		if g == gid {
			return true
		}
	}
	return false
}
