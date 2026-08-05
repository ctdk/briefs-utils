package fuse

import (
	"fmt"
	"sync"
	"testing"

	"github.com/ctdk/briefs-utils/briefs"
)

// TestConcurrentFileWrites exercises the per-inode-block locking: many
// goroutines write disjoint files concurrently. Files whose inodes share a
// 4K inode-table block (8 inodes/block) serialize on that shard's mutex; files
// in different blocks run concurrently. The race detector (-race) is the
// correctness gate: every shared-block access must be under the shard lock.
func TestConcurrentFileWrites(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	const n = 32
	// Create n files (serialized: createInDir takes the dir lock). With 8
	// inodes per 4K table block, these span 4 inode-table blocks.
	inos := make([]uint64, n)
	for i := 0; i < n; i++ {
		in, err := b.createInDir(1, fmt.Sprintf("f%02d", i), briefs.ModeFile|0o644, 1000, 1000, false)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		inos[i] = in.InodeNumber
	}

	// Each goroutine writes a unique 2-block pattern to its file and reads it
	// back. Writes to files in the same inode block serialize; different blocks
	// run concurrently.
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pat := makePattern(i+100, 2*int(b.blockSize))
			if _, err := b.writeFileData(inos[i], pat, 0); err != nil {
				t.Errorf("writeFileData %d: %v", i, err)
				return
			}
			got := readFile(t, b, inos[i], 0, int64(len(pat)))
			if !bytesEqual(got, pat) {
				t.Errorf("readback %d mismatch (len got %d want %d)", i, len(got), len(pat))
			}
		}(i)
	}
	wg.Wait()

	fsckClean(t, b, img)
}

// TestConcurrentDirOps runs concurrent create/unlink on the same parent
// directory. Dir ops serialize on the global dir lock (the shared per-op cache
// and partial-trie pool are not concurrency-safe without a buffer cache), so
// this is correctness-only: it must not deadlock or corrupt. Each goroutine
// creates a uniquely-named file and unlinks it; the directory ends empty.
func TestConcurrentDirOps(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	// A scratch parent directory.
	parent, err := b.createInDir(1, "parent", briefs.ModeDir|0o755, 1000, 1000, false)
	if err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	parentIno := parent.InodeNumber

	const n = 24
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("g%02d", i)
			if _, err := b.createInDir(parentIno, name, briefs.ModeFile|0o644, 1000, 1000, false); err != nil {
				t.Errorf("create %d: %v", i, err)
				return
			}
			if err := b.unlinkInDir(parentIno, name, false); err != nil {
				t.Errorf("unlink %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	// The parent should be empty now.
	if got := dirEntryCount(t, b, parentIno); got != 0 {
		t.Fatalf("parent not empty after concurrent create/unlink: %d entries", got)
	}
	fsckClean(t, b, img)
}

// TestConcurrentMixedFileAndDirOps runs file writes concurrently with directory
// mutations to exercise the cross-op inode-block locking (a file write and a
// dir op touching sibling inodes in the same 4K inode block).
func TestConcurrentMixedFileAndDirOps(t *testing.T) {
	mkfs := buildMkfs(t)
	img := mkfsImage(t, mkfs, 5000)
	b := openBridge(t, img)

	// Pre-create files so the file-writer goroutines have stable targets that
	// may share an inode-table block with the dir op's new children.
	const n = 16
	inos := make([]uint64, n)
	for i := 0; i < n; i++ {
		in, err := b.createInDir(1, fmt.Sprintf("m%02d", i), briefs.ModeFile|0o644, 1000, 1000, false)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		inos[i] = in.InodeNumber
	}

	var wg sync.WaitGroup
	// Half the goroutines write files; the other half churn the directory
	// (create+unlink). They may share inode-table blocks with the files, so the
	// inode-block shard lock must serialize the file write against the dir op.
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				pat := makePattern(i+200, int(b.blockSize))
				if _, err := b.writeFileData(inos[i], pat, 0); err != nil {
					t.Errorf("write %d: %v", i, err)
					return
				}
				if got := readFile(t, b, inos[i], 0, int64(len(pat))); !bytesEqual(got, pat) {
					t.Errorf("readback %d mismatch", i)
				}
			} else {
				name := fmt.Sprintf("c%02d", i)
				if _, err := b.createInDir(1, name, briefs.ModeFile|0o644, 1000, 1000, false); err != nil {
					t.Errorf("create %d: %v", i, err)
					return
				}
				if err := b.unlinkInDir(1, name, false); err != nil {
					t.Errorf("unlink %d: %v", i, err)
				}
			}
		}(i)
	}
	wg.Wait()

	// The pre-created m-files all remain (writes don't remove them); the
	// c-files were created then unlinked (net zero). So the root still holds
	// all n pre-created entries.
	if got := dirEntryCount(t, b, 1); got != n {
		t.Fatalf("root entries after mixed ops: want %d, got %d", n, got)
	}
	fsckClean(t, b, img)
}