package mcp

import (
	"strings"
	"testing"
)

func TestGetSymbolWrongPathIsNotAmbiguousWhenOneDefinitionExists(t *testing.T) {
	fs := newFullServer(t, seedChain)
	text := resultText(callTool(t, fs.sess, "get_symbol", map[string]any{"name": "entry", "path": "nowhere/other.go"}))
	if strings.Contains(text, "AMBIGUOUS") {
		t.Errorf("one definition exists, so nothing is ambiguous:\n%s", text)
	}
	if !strings.Contains(text, "svc/flow.go") || !strings.Contains(text, "nowhere/other.go") {
		t.Errorf("the answer must name the path asked for and the definition that exists:\n%s", text)
	}
}

func TestClipEchoedKeepsWordsAndCutsHugeTokens(t *testing.T) {
	if got := clipEchoed("no symbol named \"abc\" in the index"); got != "no symbol named \"abc\" in the index" {
		t.Errorf("ordinary text changed: %q", got)
	}
	got := clipEchoed("no symbol named \"" + strings.Repeat("x", 5000) + "\" in the index")
	if len(got) > 400 || !strings.Contains(got, "…") || !strings.HasSuffix(got, " in the index") {
		t.Errorf("a huge token was not cut: %d bytes", len(got))
	}
}

func TestHugeNameIsNotEchoedInFull(t *testing.T) {
	fs := newFullServer(t, seedChain)
	huge := strings.Repeat("n", 2<<20)
	for _, tool := range []string{"get_symbol", "find_symbol", "find_callers", "search_code"} {
		args := map[string]any{"name": huge}
		if tool == "search_code" {
			args = map[string]any{"query": huge}
		}
		text := resultText(callTool(t, fs.sess, tool, args))
		if len(text) > 16<<10 {
			t.Errorf("%s echoed %d bytes of a %d-byte name", tool, len(text), len(huge))
		}
	}
}
