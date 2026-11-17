package mcp

import (
	"strings"
	"testing"
)

// A function whose source says what not to break says it in the answer that
// shows the function: get_symbol and explore_symbol both carry the note.
func TestMarkedCommentsReachTheAnswer(t *testing.T) {
	src := "package pay\n\n" +
		"// HACK: the gateway returns 200 on a declined card; read the body.\n" +
		"func Charge() error {\n" +
		"\tx := 1 // NOTE(race): guarded by mu\n" +
		"\treturn nil\n" +
		"}\n\n" +
		"func Plain() {}\n"
	fs := serverFor(t, func(b *indexBuilder) {
		b.fileText("pay/charge.go", "go", src, fn("Charge", 4, 7), fn("Plain", 9, 9))
		b.wellBound("go", ".go")
	})
	for _, tc := range []struct{ tool, arg string }{{"get_symbol", "name"}, {"explore_symbol", "name"}, {"plan_change", "symbol"}} {
		tool := tc.tool
		text, _ := answer(t, fs.sess, tool, map[string]any{tc.arg: "Charge"})
		for _, want := range []string{
			"Read before changing Charge: the source marks 2 comments.",
			"pay/charge.go:3 HACK: the gateway returns 200 on a declined card; read the body.",
			"pay/charge.go:5 NOTE(race): guarded by mu",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("%s: missing %q in:\n%s", tool, want, text)
			}
		}
	}
	text, _ := answer(t, fs.sess, "get_symbol", map[string]any{"name": "Plain"})
	if strings.Contains(text, "Read before changing") {
		t.Errorf("a function with no marked comment got a note:\n%s", text)
	}
}
