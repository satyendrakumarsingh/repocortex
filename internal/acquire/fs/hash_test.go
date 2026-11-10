package fs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHashBytesMatchesHashFile is the guard that keeps incremental indexing
// correct.
//
// The walker hashes by streaming from disk; incremental re-indexing hashes
// bytes already in memory. If the two ever diverge, every file compares as
// modified, incremental indexing quietly becomes a full re-index every time,
// and nothing fails to tell you. Cheap test, expensive bug.
func TestHashBytesMatchesHashFile(t *testing.T) {
	dir := t.TempDir()

	cases := map[string][]byte{
		"empty.txt":    {},
		"small.go":     []byte("package p\n"),
		"unicode.go":   []byte("package p // ünïcödé ✓ 日本語\n"),
		"newlines.txt": []byte("a\r\nb\nc\r\n"),
		// Larger than the 32 KiB streaming buffer, so the multi-read path is
		// exercised rather than a single Read.
		"big.txt": []byte(strings.Repeat("abcdefghij", 20_000)),
	}

	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, content, 0o644); err != nil {
				t.Fatal(err)
			}

			streamed, binary, err := hashFile(path, DefaultMaxFileSize)
			if err != nil {
				t.Fatalf("hashFile: %v", err)
			}
			if binary {
				t.Fatalf("%s was classified binary", name)
			}

			inMemory := HashBytes(content)
			if streamed != inMemory {
				t.Fatalf("hash mismatch for %s:\n  streamed = %s\n  in-memory= %s\n"+
					"incremental indexing would treat every file as modified", name, streamed, inMemory)
			}
		})
	}
}

func TestHashBytesIsStable(t *testing.T) {
	content := []byte("package p\n\nfunc A() {}\n")
	first := HashBytes(content)
	for i := 0; i < 100; i++ {
		if got := HashBytes(content); got != first {
			t.Fatalf("hash changed between calls: %s vs %s", first, got)
		}
	}
	if len(first) != 64 {
		t.Errorf("hash length = %d, want 64 hex chars", len(first))
	}
}

func TestHashBytesDistinguishesContent(t *testing.T) {
	a := HashBytes([]byte("package p\nfunc A() {}\n"))
	b := HashBytes([]byte("package p\nfunc B() {}\n"))
	if a == b {
		t.Fatal("different content produced the same hash")
	}
}
