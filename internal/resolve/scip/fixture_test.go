package scip

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	scippb "github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// No real .scip fixture ships with this repository: producing one requires
// running scip-go or scip-java, which needs a toolchain the test suite must not
// depend on (CONTRIBUTING: no C compiler, no external tools). The fixtures here
// are therefore generated programmatically with the scip Go bindings and
// written in the same wire format an indexer emits — length-delimited
// submessages on Index fields 1, 2 and 3 — so ParseStreaming exercises exactly
// the code path a real file would.

const (
	fieldMetadata       protowire.Number = 1
	fieldDocument       protowire.Number = 2
	fieldExternalSymbol protowire.Number = 3
)

// writeMessage appends one length-delimited submessage, mirroring how protobuf
// serialises a repeated field. Writing the index incrementally rather than
// marshalling a whole scippb.Index keeps fixture generation itself streaming,
// which matters for the large fixtures the benchmarks build.
func writeMessage(w io.Writer, field protowire.Number, m proto.Message) error {
	body, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	var hdr []byte
	hdr = protowire.AppendTag(hdr, field, protowire.BytesType)
	hdr = protowire.AppendVarint(hdr, uint64(len(body)))
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

func writeIndex(t testing.TB, w io.Writer, meta *scippb.Metadata, docs ...*scippb.Document) {
	t.Helper()
	if meta != nil {
		if err := writeMessage(w, fieldMetadata, meta); err != nil {
			t.Fatalf("write metadata: %v", err)
		}
	}
	for _, d := range docs {
		if err := writeMessage(w, fieldDocument, d); err != nil {
			t.Fatalf("write document %q: %v", d.GetRelativePath(), err)
		}
	}
}

func indexBytes(t testing.TB, meta *scippb.Metadata, docs ...*scippb.Document) []byte {
	t.Helper()
	var buf bytes.Buffer
	writeIndex(t, &buf, meta, docs...)
	return buf.Bytes()
}

func metadata(root string) *scippb.Metadata {
	return &scippb.Metadata{
		Version:     scippb.ProtocolVersion_UnspecifiedProtocolVersion,
		ProjectRoot: root,
		ToolInfo:    &scippb.ToolInfo{Name: "scip-go", Version: "v0.9.9"},
	}
}

func rng(startLine, startChar, endLine, endChar int32) scippb.Range {
	return scippb.Range{
		Start: scippb.Position{Line: startLine, Character: startChar},
		End:   scippb.Position{Line: endLine, Character: endChar},
	}
}

// def builds a definition occurrence with a body range, which is what lets the
// ingester attribute references to an enclosing symbol.
func def(symbol string, name scippb.Range, body scippb.Range) *scippb.Occurrence {
	o := &scippb.Occurrence{Symbol: symbol, SymbolRoles: int32(scippb.SymbolRole_Definition)}
	o.SetSourceRange(name)
	o.SetEnclosingSourceRange(body)
	return o
}

func ref(symbol string, at scippb.Range, roles ...scippb.SymbolRole) *scippb.Occurrence {
	var bits int32
	for _, r := range roles {
		bits |= int32(r)
	}
	o := &scippb.Occurrence{Symbol: symbol, SymbolRoles: bits}
	o.SetSourceRange(at)
	return o
}

// goSymbol builds a scip-go style global symbol string.
func goSymbol(suffix string) string {
	return "scip-go gomod example.com/repo v1.0.0 " + suffix
}

// syntheticIndex builds an index of nDocs documents with perOccurrences
// reference occurrences each, for throughput and memory measurement.
func syntheticIndex(t testing.TB, w io.Writer, nDocs, perDoc int) (paths []string, occurrences int) {
	t.Helper()
	if err := writeMessage(w, fieldMetadata, metadata("file:///build/repo")); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
	for i := range nDocs {
		path := fmt.Sprintf("internal/pkg%03d/file%04d.go", i%64, i)
		paths = append(paths, path)
		d := &scippb.Document{
			RelativePath:     path,
			Language:         "go",
			PositionEncoding: scippb.PositionEncoding_UTF8CodeUnitOffsetFromLineStart,
		}
		owner := goSymbol(fmt.Sprintf("pkg%03d/Type%04d#", i%64, i))
		d.Symbols = append(d.Symbols, &scippb.SymbolInformation{
			Symbol:      owner,
			Kind:        scippb.SymbolInformation_Struct,
			DisplayName: fmt.Sprintf("Type%04d", i),
		})
		d.Occurrences = append(d.Occurrences,
			def(owner, rng(0, 5, 0, 20), rng(0, 0, int32(perDoc)+2, 0)))
		for j := range perDoc {
			target := goSymbol(fmt.Sprintf("pkg%03d/Type%04d#method%02d().", (i+1)%64, (i+7)%nDocs, j%16))
			d.Occurrences = append(d.Occurrences,
				ref(target, rng(int32(j)+1, 4, int32(j)+1, 24), scippb.SymbolRole_ReadAccess))
		}
		occurrences += perDoc + 1
		if err := writeMessage(w, fieldDocument, d); err != nil {
			t.Fatalf("write document: %v", err)
		}
	}
	return paths, occurrences
}
