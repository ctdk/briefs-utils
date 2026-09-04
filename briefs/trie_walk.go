package briefs

import (
	"errors"
	"fmt"
)

// TrieReadFunc reads one whole trie page (block) into a fresh buffer.
// fsck adapts os.File.ReadAt and the FUSE bridge adapts
// BlockDevice.ReadBlock to it; the walk owns everything above this call.
type TrieReadFunc func(block uint64) ([]byte, error)

// TrieWalkNote classifies the recoverable problems a trie walk can hit.
// The consumer decides per kind whether to abort the walk (returning the
// error from its Note hook) or skip the offending node/chain and carry on
// with the rest of the walk.
type TrieWalkNote int

const (
	// TrieNoteRead: the node's page could not be read from the device.
	TrieNoteRead TrieWalkNote = iota
	// TrieNotePage: bad trie page header (magic, short buffer).
	TrieNotePage
	// TrieNoteSlot: node slot index out of range or unparsable.
	TrieNoteSlot
	// TrieNoteCycle: first_child back-edge re-reaching a node the walk has
	// already visited (and not as a leaf re-visit).
	TrieNoteCycle
	// TrieNoteSiblingCap: a sibling chain longer than TrieSiblingMax
	// (corrupt/cyclic trie). The chain gathered so far is kept.
	TrieNoteSiblingCap
	// TrieNoteSiblingRead: a sibling page could not be read or parsed
	// while following a chain. The chain gathered so far is kept.
	TrieNoteSiblingRead
)

// ErrSkipTrieNode is the sentinel a TrieVisitor.VisitNode returns to drop
// the node's leaf entry without aborting the walk. The node's children
// are still walked (they may hold unrelated live entries); callers that
// want nothing to do with a corrupt node's subtree should abort with a
// real error instead.
var ErrSkipTrieNode = errors.New("skip trie node")

// TrieWalker is a resumable depth-first walk over a directory trie.
//
// It owns the traversal mechanics shared by every trie consumer: the
// dynamic visit stack (a 255-byte name can push up to TrieSiblingMax
// siblings per level, which overflows any fixed array), the visited-node
// cycle check, the leaf re-visit used to walk the children of a leaf that
// also branches, and the TrieSiblingMax cap on sibling chains with cycle
// protection (kernel trie.c caps the same walks with -EIO).
//
// Next surfaces every node the walk reaches, including the post-entry
// re-visit of a leaf with children (emitted=true); consumers that only
// want entries simply skip those. This is what lets a readdir iterator
// stop after a single entry (the FUSE bridge's dirIsEmpty) without
// walking the whole trie.
type TrieWalker struct {
	read  TrieReadFunc
	stack []uint64
	emit  []bool
	// visited holds every node already reached once (leaf re-visits are
	// distinguished by the emit flag, so a node may legitimately appear
	// twice but never three times).
	visited map[uint64]bool
	// Note, if set, is called on every recoverable problem. A non-nil
	// return aborts the walk: Next reports ok=false from then on. A nil
	// Note skips the offending node/chain silently.
	Note func(ref uint64, kind TrieWalkNote, err error) error
	done bool
}

// NewTrieWalker returns a walker for the trie rooted at root. A null root
// yields a walk that is immediately finished.
func NewTrieWalker(read TrieReadFunc, root uint64) *TrieWalker {
	w := &TrieWalker{
		read:    read,
		visited: make(map[uint64]bool),
	}
	if !TrieRefIsNull(root) {
		w.stack = append(w.stack, root)
		w.emit = append(w.emit, false)
	}
	return w
}

// note routes a problem through the hook and records an abort. It returns
// true when the walk is over.
func (w *TrieWalker) note(ref uint64, kind TrieWalkNote, err error) bool {
	if w.Note == nil {
		return false
	}
	if nerr := w.Note(ref, kind, err); nerr != nil {
		w.done = true
		return true
	}
	return false
}

// Next advances the walk one step. ok=false means the walk is finished
// (empty stack, unreadable start, or a Note that aborted). Each node is
// returned as it is reached, in the same first-child-first order the
// on-disk sibling chains encode; the subtree under a returned node is
// scheduled before the node is handed back, so entry order matches the
// previous hand-rolled walks exactly.
func (w *TrieWalker) Next() (ref uint64, emitted bool, buf []byte, page *TriePage, node *TrieSlot, ok bool) {
	for !w.done && len(w.stack) > 0 {
		ref = w.stack[len(w.stack)-1]
		emitted = w.emit[len(w.emit)-1]
		w.stack = w.stack[:len(w.stack)-1]
		w.emit = w.emit[:len(w.emit)-1]

		if w.visited[ref] && !emitted {
			// first_child back-edge: skip it and drain the rest of the
			// stack. fsck reports these; readdir just ignores them.
			if w.note(ref, TrieNoteCycle, nil) {
				return 0, false, nil, nil, nil, false
			}
			continue
		}
		if !emitted {
			w.visited[ref] = true
		}

		buf, page, node, ok = w.readNode(ref)
		if !ok {
			continue
		}

		// Schedule the subtree. A leaf that also branches is re-visited
		// once (emitted=true) so its children are pushed after the leaf
		// entry has been consumed.
		switch {
		case emitted:
			w.pushChildren(ref, node)
		case TrieIsLeaf(node.NodeType):
			if !TrieRefIsNull(node.FirstChild) {
				w.stack = append(w.stack, ref)
				w.emit = append(w.emit, true)
			}
		default:
			w.pushChildren(ref, node)
		}
		return ref, emitted, buf, page, node, true
	}
	return 0, false, nil, nil, nil, false
}

// readNode reads and parses the page and slot a reference points at.
func (w *TrieWalker) readNode(ref uint64) (buf []byte, page *TriePage, node *TrieSlot, ok bool) {
	buf, err := w.read(TrieRefBlock(ref))
	if err != nil {
		w.note(ref, TrieNoteRead, err)
		return nil, nil, nil, false
	}
	page, err = ReadTriePage(buf)
	if err != nil {
		w.note(ref, TrieNotePage, err)
		return nil, nil, nil, false
	}
	node, err = ReadTrieSlot(buf, TrieRefSlot(ref))
	if err != nil {
		w.note(ref, TrieNoteSlot, err)
		return nil, nil, nil, false
	}
	return buf, page, node, true
}

// pushChildren gathers a node's child chain and pushes it so the first
// child pops first.
func (w *TrieWalker) pushChildren(parentRef uint64, node *TrieSlot) {
	children := w.collectChildren(parentRef, node)
	for i := len(children) - 1; i >= 0; i-- {
		w.stack = append(w.stack, children[i])
		w.emit = append(w.emit, false)
	}
}

// collectChildren follows a node's sibling chain, gathering child
// references. The chain is capped at TrieSiblingMax hops: a back-edge in
// next_sibling (corrupt/stale trie) ends the chain (optionally reported
// through Note) instead of spinning forever. A child that cannot be read
// or parsed ends the chain the same way.
func (w *TrieWalker) collectChildren(parentRef uint64, node *TrieSlot) []uint64 {
	var children []uint64
	child := node.FirstChild
	for hops := 0; !TrieRefIsNull(child); hops++ {
		if hops >= TrieSiblingMax {
			w.note(parentRef, TrieNoteSiblingCap,
				fmt.Errorf("sibling chain from ref %d exceeds %d nodes", parentRef, TrieSiblingMax))
			break
		}
		children = append(children, child)
		cbuf, rerr := w.read(TrieRefBlock(child))
		if rerr == nil {
			if _, perr := ReadTriePage(cbuf); perr != nil {
				rerr = perr
			} else if cn, serr := ReadTrieSlot(cbuf, TrieRefSlot(child)); serr != nil {
				rerr = serr
			} else {
				child = cn.NextSibling
				continue
			}
		}
		w.note(parentRef, TrieNoteSiblingRead, rerr)
		break
	}
	return children
}

// TrieVisitor receives the nodes of one eager trie walk (WalkTrie).
type TrieVisitor struct {
	// VisitNode is called for every node the walk reaches, in DFS order,
	// including the post-entry re-visit of a leaf that also branches
	// (emitted=true). Return ErrSkipTrieNode to skip just this node's
	// leaf entry; any other non-nil error aborts the walk.
	VisitNode func(ref uint64, emitted bool, buf []byte, page *TriePage, node *TrieSlot) error

	// VisitLeaf is called for each node carrying a leaf entry (after
	// VisitNode, and only on its first, non-emitted visit). The name is
	// read with ReadTrieName; filtering NODE_FLAG_DELETED entries is the
	// visitor's business (the kernel never writes that flag today).
	VisitLeaf func(ref uint64, buf []byte, node *TrieSlot) error

	// Note receives the walk's recoverable problems. A non-nil return
	// aborts the walk and becomes WalkTrie's return value.
	Note func(ref uint64, kind TrieWalkNote, err error) error
}

// WalkTrie walks a directory trie rooted at root, handing every node to
// the visitor. It is the shared body of the four walks that used to be
// hand-rolled (fsck verification/collection and the FUSE readdir
// iterator, which instead wraps the lazy TrieWalker directly).
func WalkTrie(read TrieReadFunc, root uint64, v TrieVisitor) error {
	w := NewTrieWalker(read, root)
	var abort error
	w.Note = func(ref uint64, kind TrieWalkNote, err error) error {
		if v.Note == nil {
			return nil
		}
		if nerr := v.Note(ref, kind, err); nerr != nil {
			abort = nerr
			return nerr
		}
		return nil
	}
	for {
		ref, emitted, buf, page, node, ok := w.Next()
		if !ok {
			break
		}
		if v.VisitNode != nil {
			if err := v.VisitNode(ref, emitted, buf, page, node); err != nil {
				if err == ErrSkipTrieNode {
					continue
				}
				return err
			}
		}
		if !emitted && v.VisitLeaf != nil && TrieIsLeaf(node.NodeType) {
			if err := v.VisitLeaf(ref, buf, node); err != nil {
				return err
			}
		}
	}
	return abort
}
