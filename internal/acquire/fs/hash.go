package fs

import (
	"crypto/sha256"
	"encoding/hex"
)

// HashBytes returns the content hash for bytes already held in memory.
//
// Why this is exported, and why it lives beside hashFile: incremental indexing
// compares a hash produced by the walker (which streams from disk) against one
// produced while re-indexing a single changed file (which already has the
// bytes). If those two ever disagree — a different algorithm, a different
// encoding, a length prefix — then every file would compare as modified and
// incremental indexing would silently degrade into a full re-index on every
// pass, with nothing failing to signal it.
//
// So there is exactly one definition of "the content hash", and
// TestHashBytesMatchesHashFile pins the streaming path to it.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
