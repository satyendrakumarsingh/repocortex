package web

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The canvas renderer's two traps, verified BY EXECUTION.
//
// # Why this test is unusual
//
// Everything else in this package is Go asserting on Go. This one runs
// JavaScript, because the two guards it checks are behaviour rather than shape
// and a Go test that grepped assets/app.js for the string "preventDefault"
// would pass on code that called it in the wrong branch. Both guards were
// written, commented, and never once executed before this test existed:
//
//	measure()  returns early when getBoundingClientRect() is under 1x1. A canvas
//	           inside a `hidden` panel measures 0x0, and a fit computed from 0x0
//	           pins the graph at minimum zoom in a corner — permanently, because
//	           nothing recomputes it when the panel is shown.
//
//	wheel      returns BEFORE preventDefault unless ctrl or meta is held. A
//	           canvas that swallows every wheel event traps the page: a reader
//	           scrolling past the graph gets stuck zooming it.
//
// # The toolchain constraint, honoured
//
// The portal ships with no npm, no bundler and no build step, and this test
// does not change that. It installs nothing and bundles nothing: it runs
// testdata/canvas_harness.mjs on whatever `node` is already on PATH, and SKIPS
// with an explanation when there is not one. A machine without node still gets
// a green build; it simply does not get this proof, and the skip message says
// so rather than implying the check ran.
//
// # What it does NOT prove
//
// Nothing about pixels. No test in this package has ever looked at the rendered
// canvas, and this one does not either — it proves the two decisions, not the
// drawing. Colour, layout and legibility remain unverified by eye.
func TestWebCanvasTrapsHoldUnderExecution(t *testing.T) {
	t.Parallel()

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH: the canvas traps in assets/app.js are NOT verified by this run. " +
			"They are behaviour, not shape, so nothing in Go can stand in for executing them. " +
			"Install any Node runtime and re-run to gate on them.")
	}

	// Read the script out of the EMBEDDED tree, so this checks what the binary
	// actually serves rather than a file that happens to sit beside it.
	body, err := fs.ReadFile(assets, "assets/app.js")
	if err != nil {
		t.Fatalf("read embedded assets/app.js: %v", err)
	}
	script := filepath.Join(t.TempDir(), "app.js")
	if err := os.WriteFile(script, body, 0o600); err != nil {
		t.Fatalf("stage app.js: %v", err)
	}

	cmd := exec.Command(node, filepath.Join("testdata", "canvas_harness.mjs"), script)
	out, err := cmd.CombinedOutput()
	t.Logf("canvas harness:\n%s", out)
	if err != nil {
		t.Fatalf("the canvas harness failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "0 failed") {
		t.Fatalf("the canvas harness did not report a clean run:\n%s", out)
	}

	// An extraction failure must be loud. The harness asserts on the extraction
	// itself, so a reformat of measure() or of the wheel listener fails here
	// rather than quietly checking nothing.
	for _, want := range []string{
		"hidden panel (0x0)",
		"the FIRST real measurement runs the deferred fit",
		"plain wheel",
		"ctrl+wheel",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the harness did not exercise %q — it may have failed to locate the code under test", want)
		}
	}
}

// TestWebAssetsAreSelfContained pins the constraint the whole portal rests on:
// it must work offline, on a machine with no Node and no network. A CDN link,
// a remote font or an added <script src="https://…"> would work perfectly in
// development and fail on the aeroplane, so it fails here instead.
func TestWebAssetsAreSelfContained(t *testing.T) {
	t.Parallel()

	err := fs.WalkDir(assets, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, rerr := fs.ReadFile(assets, path)
		if rerr != nil {
			return rerr
		}
		text := string(body)
		for _, forbidden := range []string{"http://", "https://", "//cdn.", "@import url("} {
			// The SVG favicon carries an xmlns, which is a namespace name and
			// not a fetch. Everything else naming an absolute origin is one.
			if strings.Contains(text, forbidden) && !onlyXMLNS(text, forbidden) {
				t.Errorf("%s references %q — the portal must load nothing from another origin", path, forbidden)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded assets: %v", err)
	}
}

// onlyXMLNS reports whether every occurrence of pattern in text sits inside an
// xmlns attribute, which names a namespace rather than fetching anything.
func onlyXMLNS(text, pattern string) bool {
	for i := 0; ; {
		j := strings.Index(text[i:], pattern)
		if j < 0 {
			return true
		}
		at := i + j
		start := max(0, at-40)
		if !strings.Contains(text[start:at], "xmlns") {
			return false
		}
		i = at + len(pattern)
	}
}
