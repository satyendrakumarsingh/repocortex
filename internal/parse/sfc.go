package parse

import (
	"context"
	"regexp"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// sfcParser extracts a single-file component's <script> block and hands it to
// the parser for the language that block is actually written in.
//
// A .vue file is not one language, it is three in a trenchcoat: markup, a
// script written in TypeScript or JavaScript, and CSS. The Vue grammar parses
// the SFC envelope and leaves the script body as opaque text, so a tags query
// over it finds no functions, no imports and no components — which is why 514
// .vue files on a real Nuxt frontend were recorded as
// `no tags query available for "vue"` and contributed nothing at all.
//
// Everything a developer asks RepoCortex about a component — what does it import,
// what does it call, where is this function defined — lives in that script.
// Reading it as the language it declares is the difference between indexing the
// frontend and skipping it.
//
// Line numbers are offset back to the .vue file, so every reported location is
// a place the reader can actually open. Getting that wrong would be worse than
// skipping the file: a confident location that lands on the wrong line costs
// more than an honest gap.
type sfcParser struct {
	language string
	// delegates are chosen by the block's lang attribute, defaulting to ts.
	registry *registry
}

// scriptBlock matches an SFC <script> element, capturing its attributes and
// body. Non-greedy so that a file with several script blocks yields each in
// turn rather than one span from the first open tag to the last close.
var scriptBlock = regexp.MustCompile(`(?is)<script([^>]*)>(.*?)</script>`)

// langAttr pulls `lang="ts"` out of a script tag's attributes.
var langAttr = regexp.MustCompile(`(?i)\blang\s*=\s*["']([a-z]+)["']`)

func (p *sfcParser) Language() string { return p.language }

func (p *sfcParser) Parse(ctx context.Context, path string, src []byte) (*Result, error) {
	res := &Result{Language: p.language, Status: StatusOK}

	blocks := scriptBlock.FindAllSubmatchIndex(src, -1)
	if len(blocks) == 0 {
		// A component with no script is template and style only. That is a
		// complete parse of a file that declares nothing, not a failure.
		return res, nil
	}

	for _, b := range blocks {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		attrs := string(src[b[2]:b[3]])
		body := src[b[4]:b[5]]
		if len(strings.TrimSpace(string(body))) == 0 {
			continue
		}

		inner := p.registry.ForFile("sfc." + scriptExt(attrs))
		if inner == nil {
			continue
		}
		// Lines before the body start, so ranges can be moved back into the
		// .vue file's own coordinates.
		offset := strings.Count(string(src[:b[4]]), "\n")

		sub, err := inner.Parse(ctx, path, body)
		if err != nil || sub == nil {
			// One unparsable block must not discard the others, and must not
			// fail the file: the component still exists.
			res.Status = StatusPartial
			continue
		}
		if sub.Status == StatusPartial || sub.Status == StatusFailed {
			res.Status = StatusPartial
		}
		mergeShifted(res, sub, offset)
	}
	return res, nil
}

// scriptExt maps a script block's lang attribute to a file extension the
// registry can resolve. TypeScript is the default because `<script setup>` in a
// TypeScript project carries no lang attribute, and parsing TS source as
// JavaScript loses every type declaration in it.
func scriptExt(attrs string) string {
	m := langAttr.FindStringSubmatch(attrs)
	if m == nil {
		return "ts"
	}
	switch strings.ToLower(m[1]) {
	case "js", "javascript":
		return "js"
	case "jsx":
		return "jsx"
	case "tsx":
		return "tsx"
	default:
		return "ts"
	}
}

// mergeShifted appends sub's facts to res, moving every line into the outer
// file's coordinates.
func mergeShifted(res, sub *Result, offset int) {
	for _, s := range sub.Symbols {
		s.Range.Start.Line += offset
		s.Range.End.Line += offset
		res.Symbols = append(res.Symbols, s)
	}
	for _, r := range sub.References {
		r.Range.Start.Line += offset
		r.Range.End.Line += offset
		res.References = append(res.References, r)
	}
	for _, i := range sub.Imports {
		i.Range.Start.Line += offset
		i.Range.End.Line += offset
		res.Imports = append(res.Imports, i)
	}
}

// interface assertion: sfcParser is a Parser.
var _ Parser = (*sfcParser)(nil)

// unusedModelRef keeps the model import honest if the merge helpers change.
var _ = model.Symbol{}
