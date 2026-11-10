package cli

import (
	"io"
	"strings"
	"testing"
)

// TestJSONLineFilterDropsOnlyMalformedLines is the property the MCP session
// depends on: a stray line from a client must vanish, and every valid frame
// around it must survive byte-for-byte and in order.
func TestJSONLineFilterDropsOnlyMalformedLines(t *testing.T) {
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize"}
not json at all
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
   
{"jsonrpc":"2.0","id":3,"method":"ping"}
`
	got, err := io.ReadAll(newJSONLineFilter(strings.NewReader(in), nil))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	out := string(got)

	for _, want := range []string{`"id":1`, `"id":2`, `"id":3`} {
		if !strings.Contains(out, want) {
			t.Errorf("a valid frame was dropped (%s):\n%s", want, out)
		}
	}
	if strings.Contains(out, "not json at all") {
		t.Errorf("the malformed line survived:\n%s", out)
	}
	// Order must hold, or request/response correlation breaks.
	i1, i2, i3 := strings.Index(out, `"id":1`), strings.Index(out, `"id":2`), strings.Index(out, `"id":3`)
	if i1 >= i2 || i2 >= i3 {
		t.Errorf("frames were reordered: %d %d %d", i1, i2, i3)
	}
}

// TestJSONLineFilterPreservesFraming. The SDK reads newline-delimited JSON, so
// a filter that ate or added a newline would merge two frames into one.
func TestJSONLineFilterPreservesFraming(t *testing.T) {
	in := "{\"a\":1}\n{\"b\":2}\n"
	got, err := io.ReadAll(newJSONLineFilter(strings.NewReader(in), nil))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != in {
		t.Errorf("framing changed:\n want %q\n got  %q", in, string(got))
	}
}

// TestJSONLineFilterHandlesAMissingFinalNewline. A client that closes without a
// trailing newline must still have its last frame delivered.
func TestJSONLineFilterHandlesAMissingFinalNewline(t *testing.T) {
	got, err := io.ReadAll(newJSONLineFilter(strings.NewReader(`{"a":1}`), nil))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"a":1`) {
		t.Errorf("the final unterminated frame was lost: %q", string(got))
	}
}

func TestJSONLineFilterIsSafeOnHostileInput(t *testing.T) {
	for _, in := range []string{
		strings.Repeat("{", 100000),
		strings.Repeat("\n", 100000),
		strings.Repeat("bad\n", 50000),
		"\x00\x01\x02\n{\"ok\":1}\n",
		strings.Repeat(`{"a":1}`+"\n", 20000),
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic on %.20q: %v", in, r)
				}
			}()
			if _, err := io.ReadAll(newJSONLineFilter(strings.NewReader(in), nil)); err != nil {
				t.Errorf("read error on %.20q: %v", in, err)
			}
		}()
	}
}
