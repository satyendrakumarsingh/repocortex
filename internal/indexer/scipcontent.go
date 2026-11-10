package indexer

import (
	"bufio"
	"errors"
	"io"
	"os"

	"github.com/satyendrakumarsingh/repocortex/internal/errs"
)

// Why this file exists.
//
// scip-go can fail and still exit 0. When go/packages cannot load a module —
// here, because a `replace` pointed at a directory whose own go.mod required a
// newer release — scip-go reports "Visiting Packages [0/0]", writes an index
// containing metadata and nothing else, and returns success:
//
//	$ scip-go --output pg.scip; echo $?
//	Visiting Packages [0/0]
//	0
//	$ ls -l pg.scip
//	-rw-r--r-- 245 pg.scip
//
// Run's existing guard is an os.Stat, so a 245-byte index passed it and the
// failure travelled all the way to import, where it surfaced as "none of the
// index's 0 definition(s) matched a symbol in this repository". That message
// blames the repository for a tool that never looked at it.
//
// The rule this restores is that failure must never look like an empty result.
// An indexer that produced no documents did not index anything, whatever its
// exit code claimed, and Run must say so at the point it can still name the
// project and show the tool's output.
//
// WHY A HAND-ROLLED SCAN. This reads exactly one fact — "is there at least one
// document" — from the SCIP wire format:
//
//	message Index {
//	  Metadata metadata            = 1;
//	  repeated Document documents  = 2;
//	  repeated SymbolInformation external_symbols = 3;
//	}
//
// A real protobuf decode would mean this package depending on the SCIP schema
// and, through it, on internal/resolve/scip — pointing the producer at the
// consumer, which §1 forbids. It would also mean loading a 54 MB index into
// memory to answer a yes/no question. The scan below stops at the first field 2
// tag, so a healthy index costs a few hundred bytes.

// scipFieldDocuments is field 2 of the SCIP Index message.
const scipFieldDocuments = 2

// errMalformedIndex says the bytes are not a SCIP index at all.
var errMalformedIndex = errors.New("not a readable SCIP index")

// scipHasDocuments reports whether the index at path contains at least one
// document.
//
// A malformed file is reported as an error rather than as "empty": the two are
// different failures and collapsing them would put this function in the same
// business as the bug it exists to catch.
func scipHasDocuments(path string) (bool, error) {
	f, err := os.Open(path) //nolint:gosec // G304: outDir this process chose for the indexer's output; contents parsed as untrusted
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()

	r := bufio.NewReader(f)
	for {
		key, err := readVarint(r)
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		field, wire := key>>3, key&7
		if field == scipFieldDocuments {
			return true, nil
		}
		if err := skipField(r, wire); err != nil {
			return false, err
		}
	}
}

// skipField advances past one field of the given wire type.
func skipField(r *bufio.Reader, wire uint64) error {
	switch wire {
	case 0: // varint
		_, err := readVarint(r)
		return err
	case 1: // 64-bit
		return discard(r, 8)
	case 2: // length-delimited
		n, err := readVarint(r)
		if err != nil {
			return err
		}
		// A length that cannot be a real message is a corrupt file, and
		// discarding it would be an unbounded read driven by file contents.
		if n > 1<<40 {
			return errMalformedIndex
		}
		return discard(r, int64(n))
	case 5: // 32-bit
		return discard(r, 4)
	default:
		// Groups (3, 4) were removed from proto3 and never appear in SCIP.
		return errMalformedIndex
	}
}

func discard(r *bufio.Reader, n int64) error {
	if _, err := io.CopyN(io.Discard, r, n); err != nil {
		if errors.Is(err, io.EOF) {
			return errMalformedIndex
		}
		return err
	}
	return nil
}

// readVarint reads one base-128 varint. Ten bytes is the maximum encoding of a
// 64-bit value; anything longer is corrupt, not merely large.
func readVarint(r *bufio.Reader) (uint64, error) {
	var v uint64
	for i := 0; i < 10; i++ {
		b, err := r.ReadByte()
		if err != nil {
			if i > 0 && errors.Is(err, io.EOF) {
				return 0, errMalformedIndex
			}
			return 0, err
		}
		v |= uint64(b&0x7f) << (7 * i)
		if b < 0x80 {
			return v, nil
		}
	}
	return 0, errMalformedIndex
}

// emptyIndexError is the error Run returns for an index with no documents.
func emptyIndexError(ix Indexer, p Project) error {
	return errs.New(errs.KindUnsupported, "indexer.Run",
		string(ix)+" exited 0 but produced an index with no documents for "+p.String()).
		WithField("language", string(p.Language)).
		WithField("project_dir", p.Dir)
}
