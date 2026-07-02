package main

import (
	"encoding/binary"
	"strings"
	"testing"
)

// makeTriePageBuf returns a 4096-byte page buffer with the name bytes stored at
// the end of the block. nameOffset is measured from the end of the block to
// the 2-byte length prefix.
func makeTriePageBuf(name string, nameOffset uint16) []byte {
	buf := make([]byte, 4096)
	nameLen := len(name)
	prefixOff := len(buf) - int(nameOffset)
	binary.LittleEndian.PutUint16(buf[prefixOff:], uint16(nameLen))
	copy(buf[prefixOff+2:], name)
	return buf
}

// TestExtractTrieNodeNameLengths verifies that extractTrieNodeName accepts the
// maximum kernel name length (255 bytes) and rejects over-long or inconsistent
// name_len values.
func TestExtractTrieNodeNameLengths(t *testing.T) {
	maxName := strings.Repeat("a", 255)

	t.Run("max-name", func(t *testing.T) {
		node := trieSlot{NameLen: 257, NameOffset: uint16(len(maxName) + 2)}
		buf := makeTriePageBuf(maxName, node.NameOffset)
		got := extractTrieNodeName(buf, node)
		if got != maxName {
			t.Errorf("max-name: got len=%d, want len=%d", len(got), len(maxName))
		}
	})

	t.Run("name-len-too-large", func(t *testing.T) {
		// NameLen is stored as 2 + actual length; 258 exceeds the maximum of 257.
		node := trieSlot{NameLen: 258, NameOffset: 4}
		buf := makeTriePageBuf("ab", node.NameOffset)
		got := extractTrieNodeName(buf, node)
		if got != "" {
			t.Errorf("expected empty name for NameLen 258, got %q", got)
		}
	})

	t.Run("stored-len-mismatch", func(t *testing.T) {
		node := trieSlot{NameLen: 10, NameOffset: 6}
		buf := makeTriePageBuf("abc", node.NameOffset)
		got := extractTrieNodeName(buf, node)
		if got != "" {
			t.Errorf("expected empty name when stored length mismatches NameLen, got %q", got)
		}
	})
}
