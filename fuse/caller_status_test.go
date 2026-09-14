package fuse

import (
	"context"
	"os"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// injectCallerStatus replaces the /proc reader for the duration of a test.
// The restore runs first so a fatal before it still restores the reader.
func injectCallerStatus(t *testing.T, st callerStatus, ok bool) {
	t.Helper()
	saved := procCallerStatus
	procCallerStatus = func(pid uint32) (callerStatus, bool) {
		return st, ok
	}
	t.Cleanup(func() { procCallerStatus = saved })
}

// callerCtx builds a context carrying a FUSE caller, the way go-fuse does
// for live requests.
func callerCtx(uid, gid, pid uint32) context.Context {
	return fuse.NewContext(context.Background(), &fuse.Caller{
		Owner: fuse.Owner{Uid: uid, Gid: gid},
		Pid:   pid,
	})
}

func TestCallerHasCap(t *testing.T) {
	const capsBoth = 1<<capFSetIDBit | 1<<capSysAdminBit
	injectCallerStatus(t, callerStatus{capEff: capsBoth}, true)

	ctx := callerCtx(1000, 1000, 1234)
	if !loadCallerCheck(ctx).hasCap(capFSetIDBit) {
		t.Fatalf("CAP_FSETID reported missing")
	}
	if !callerCapSysAdmin(ctx) {
		t.Fatalf("CAP_SYS_ADMIN reported missing")
	}

	injectCallerStatus(t, callerStatus{capEff: 1 << capSysAdminBit}, true)
	if loadCallerCheck(ctx).hasCap(capFSetIDBit) {
		t.Fatalf("CAP_FSETID reported held")
	}

	// No caller in the context (unit tests, direct internal calls):
	// unprivileged.
	if loadCallerCheck(context.Background()).hasCap(capFSetIDBit) {
		t.Fatalf("no-caller context reported privileged")
	}
	// Caller with pid 0: unprivileged.
	if loadCallerCheck(callerCtx(0, 0, 0)).hasCap(capSysAdminBit) {
		t.Fatalf("pid 0 caller reported privileged")
	}
	// Unreadable /proc (exited caller, different pid namespace):
	// unprivileged.
	injectCallerStatus(t, callerStatus{}, false)
	if loadCallerCheck(ctx).hasCap(capFSetIDBit) {
		t.Fatalf("unreadable status reported privileged")
	}
}

func TestCallerUmask(t *testing.T) {
	injectCallerStatus(t, callerStatus{umask: 0o022}, true)
	if got := callerUmask(callerCtx(1000, 1000, 1234)); got != 0o022 {
		t.Fatalf("umask: want 022, got %o", got)
	}
	if got := callerUmask(context.Background()); got != 0 {
		t.Fatalf("no-caller umask: want 0, got %o", got)
	}
	injectCallerStatus(t, callerStatus{}, false)
	if got := callerUmask(callerCtx(1000, 1000, 1234)); got != 0 {
		t.Fatalf("unreadable umask: want 0, got %o", got)
	}
}

func TestCallerInGroup(t *testing.T) {
	injectCallerStatus(t, callerStatus{groups: []uint32{1001, 1002}}, true)

	// egid match (the FUSE header's Gid).
	if !loadCallerCheck(callerCtx(1000, 1002, 1234)).inGroup(1002) {
		t.Fatalf("egid match reported not-in-group")
	}
	// Supplementary-group match.
	if !loadCallerCheck(callerCtx(1000, 1000, 1234)).inGroup(1001) {
		t.Fatalf("supplementary-group match reported not-in-group")
	}
	if loadCallerCheck(callerCtx(1000, 1000, 1234)).inGroup(5) {
		t.Fatalf("no match reported in-group")
	}
	// No caller / unreadable: not-in-group.
	if loadCallerCheck(context.Background()).inGroup(0) {
		t.Fatalf("no-caller context reported in-group(0)")
	}
	injectCallerStatus(t, callerStatus{}, false)
	if loadCallerCheck(callerCtx(1000, 1000, 1234)).inGroup(0) {
		t.Fatalf("unreadable status reported in-group(0)")
	}
}

func TestReadProcCallerStatus(t *testing.T) {
	// The real parser against this process's own status file. CapEff
	// depends on privileges and groups on the login session, so only the
	// umask is asserted (read back through the syscall), plus readability.
	if _, ok := readProcCallerStatus(0); ok {
		t.Fatalf("pid 0 unexpectedly readable")
	}
	st, ok := readProcCallerStatus(uint32(os.Getpid()))
	if !ok {
		t.Fatalf("own /proc status unreadable")
	}
	cur := syscall.Umask(0)
	syscall.Umask(cur)
	if st.umask != uint32(cur) {
		t.Fatalf("umask: /proc says %o, syscall says %o", st.umask, cur)
	}
}
