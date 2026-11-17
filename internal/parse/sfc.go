package parse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	ts "github.com/odvcencio/gotreesitter"
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
//
// An attribute value may contain `>`: `<script setup lang="ts" generic="T extends
// Array<string>">` is a Vue 3.3 generic component, and `[^>]*` ended the tag at
// the `>` inside the value, so the body began in the middle of the attribute
// and the script parsed as garbage. Quoted values are matched as a unit.
var scriptBlock = regexp.MustCompile(`(?is)<script((?:"[^"]*"|'[^']*'|[^>"'])*)>(.*?)</script>`)

// styleBlock matches a <style> element; template analysis must not read it.
var styleBlock = regexp.MustCompile(`(?is)<style(?:"[^"]*"|'[^']*'|[^>"'])*>.*?</style>`)

// scriptBlocks returns the submatch indexes of every <script> element that is
// markup, in order. An element inside an HTML comment (a commented-out script,
// or a script shown as an example) is not one.
func scriptBlocks(src []byte) [][]int {
	var out [][]int
	pos := 0
	for pos < len(src) {
		rest := src[pos:]
		loc := scriptBlock.FindSubmatchIndex(rest)
		if loc == nil {
			break
		}
		if c := bytes.Index(rest[:loc[0]], []byte("<!--")); c >= 0 {
			end := bytes.Index(rest[c+4:], []byte("-->"))
			if end < 0 {
				break
			}
			pos += c + 4 + end + 3
			continue
		}
		for i := range loc {
			if loc[i] >= 0 {
				loc[i] += pos
			}
		}
		out = append(out, loc)
		pos = loc[1]
	}
	return out
}

// langAttr pulls `lang="ts"` out of a script tag's attributes.
var langAttr = regexp.MustCompile(`(?i)\blang\s*=\s*["']([a-z]+)["']`)

func (p *sfcParser) Language() string { return p.language }

func (p *sfcParser) Parse(ctx context.Context, path string, src []byte) (*Result, error) {
	res := &Result{Language: p.language, Status: StatusOK}

	if len(src) == 0 {
		skip(res, model.ParseReasonEmpty, "")
		return res, nil
	}
	// The size limit is on the FILE. Each script block would pass it on its
	// own, and the regular expression below would still scan every byte of a
	// component nobody wrote by hand.
	if limit := p.registry.cfg.sourceLimit(); len(src) > limit {
		skip(res, model.ParseReasonTooLarge, tooLargeDetail(int64(len(src)), int64(limit)))
		return res, nil
	}

	blocks := scriptBlocks(src)

	// The component is itself a definition: `Button.vue` defines `Button`,
	// whatever its script holds, and a component with no script at all
	// (template and style only) is still the thing other files import.
	if c, ok := componentSymbol(path, src); ok {
		res.Symbols = append(res.Symbols, c)
	}
	if p.language == "svelte" {
		res.Symbols = append(res.Symbols, svelteSnippets(src, blocks)...)
	}
	if len(blocks) == 0 {
		// A component with no script is template and style only. That is a
		// complete parse of a file that declares nothing else, not a failure.
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

		inner := p.registry.sfcDelegate(scriptExt(attrs))
		if inner == nil {
			continue
		}
		// Lines before the body start, so ranges can be moved back into the
		// .vue file's own coordinates.
		offset := strings.Count(string(src[:b[4]]), "\n")

		sub, err := inner.Parse(ctx, path, body)
		if err != nil {
			// Only cancellation comes back as an error; a block that would
			// not parse is reported in sub.
			return nil, err
		}
		if sub == nil {
			continue
		}
		dropForeignMethods(ctx, inner, body, sub)
		adoptBlockStatus(res, sub, offset)
		mergeShifted(res, sub, offset)
	}
	res.Symbols = dropShadowedBindings(res.Symbols)
	refs, bindings := templateComponents(src, blocks, res.Imports)
	res.References = append(res.References, refs...)
	res.Imports = append(res.Imports, bindings...)
	return res, nil
}

// dropForeignMethods removes from a script block's symbols the methods of an
// object literal that is not the component's own options object.
//
// The script language's inferred query records every method written in object
// shorthand, `get data() {}` and `onClick() {}`, wherever the object is. In a
// component that is wrong for `createTable({ get data() {} })`, for
// `computed(() => ({ onClick() {} }))` and for any object bound to a name:
// those are arguments and values, not what the component defines. Methods of
// a class are untouched, and so are the methods of the options object: the
// object of `export default {}`, `defineComponent({})` or `Vue.extend({})`,
// the object of `module.exports =`, and an object one level down in it
// (`methods: { go() {} }`).
func dropForeignMethods(ctx context.Context, inner *genericParser, body []byte, sub *Result) {
	hasMethod := false
	for _, s := range sub.Symbols {
		if s.Kind == model.KindMethod {
			hasMethod = true
			break
		}
	}
	if !hasMethod {
		return
	}
	_, tree, _, err := inner.parseTree(ctx, body)
	if err != nil || tree == nil {
		return
	}
	lang := tree.Language()
	root := tree.RootNode()

	type span struct{ start, end uint32 }
	own := map[span]bool{}
	ownObject := func(n *ts.Node) {
		if n == nil || n.Type(lang) != "object" {
			return
		}
		own[span{n.StartByte(), n.EndByte()}] = true
		for i := 0; i < n.ChildCount(); i++ {
			if pair := n.Child(i); pair.Type(lang) == "pair" {
				if v := pair.ChildByFieldName("value", lang); v != nil && v.Type(lang) == "object" {
					own[span{v.StartByte(), v.EndByte()}] = true
				}
			}
		}
	}
	// the object itself, or the object a call such as defineComponent({}) is given
	optionsOf := func(v *ts.Node) {
		if v == nil {
			return
		}
		switch v.Type(lang) {
		case "object":
			ownObject(v)
		case "call_expression":
			if args := v.ChildByFieldName("arguments", lang); args != nil {
				for i := 0; i < args.ChildCount(); i++ {
					ownObject(args.Child(i))
				}
			}
		}
	}
	for i := 0; i < root.ChildCount(); i++ {
		top := root.Child(i)
		switch top.Type(lang) {
		case "export_statement":
			optionsOf(top.ChildByFieldName("value", lang))
		case "expression_statement":
			for j := 0; j < top.ChildCount(); j++ {
				assign := top.Child(j)
				if assign.Type(lang) != "assignment_expression" {
					continue
				}
				if left := assign.ChildByFieldName("left", lang); left != nil && strings.TrimSpace(left.Text(body)) == "module.exports" {
					optionsOf(assign.ChildByFieldName("right", lang))
				}
			}
		}
	}

	// Walk the tree for object-shorthand methods outside those objects.
	foreign := map[string]bool{}
	var walk func(n *ts.Node)
	walk = func(n *ts.Node) {
		if n.Type(lang) == "method_definition" {
			if parent := n.Parent(); parent != nil && parent.Type(lang) == "object" && !own[span{parent.StartByte(), parent.EndByte()}] {
				if name := n.ChildByFieldName("name", lang); name != nil {
					foreign[fmt.Sprintf("%d %s", int(n.StartPoint().Row)+1, strings.TrimSpace(name.Text(body)))] = true
				}
			}
		}
		for i := 0; i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
	if len(foreign) == 0 {
		return
	}
	kept := sub.Symbols[:0]
	for _, s := range sub.Symbols {
		if s.Kind == model.KindMethod && foreign[fmt.Sprintf("%d %s", s.Range.Start.Line, s.Name)] {
			continue
		}
		kept = append(kept, s)
	}
	sub.Symbols = kept
}

// adoptBlockStatus folds one script block's parse verdict into the file's.
//
// The component's status is the worst any of its blocks reported: a block the
// parser stopped in outranks one with syntax errors, which outranks a clean
// one. A block that failed outright leaves the component partial, not failed,
// because the other blocks and the component itself are still in the index.
// The first-error line is moved into the file's coordinates.
func adoptBlockStatus(res, sub *Result, offset int) {
	rank := func(r *Result) int {
		switch {
		case r.Status == StatusFailed:
			return 3
		case r.Status == StatusPartial && r.Reason != model.ParseReasonSyntaxErrors:
			return 2
		case r.Status == StatusPartial:
			return 1
		}
		return 0
	}
	res.ErrorNodes += sub.ErrorNodes
	res.MissingTokens += sub.MissingTokens
	if sub.FirstErrorLine > 0 {
		if line := sub.FirstErrorLine + offset; res.FirstErrorLine == 0 || line < res.FirstErrorLine {
			res.FirstErrorLine = line
		}
	}
	if sub.Status == StatusSkipped || rank(sub) < rank(res) || rank(sub) == 0 {
		return
	}
	res.Status = StatusPartial
	switch {
	case sub.Status == StatusFailed:
		// A block the parser produced nothing for: the component's index
		// entry stops short, which is what stopped_early says.
		res.Reason = model.ParseReasonStoppedEarly
		detail := "a script block could not be parsed"
		if sub.Err != nil {
			detail += ": " + sub.Err.Error()
		}
		res.Err = errors.New(model.FormatParseError(res.Reason, detail))
	case sub.Reason == model.ParseReasonSyntaxErrors:
		res.Reason = sub.Reason
		res.Err = errors.New(model.FormatParseError(res.Reason, fmt.Sprintf(
			"%s and %s in the script, first at line %d; definitions inside the regions the parser could not read may be missing",
			plural(res.ErrorNodes, "error node"), plural(res.MissingTokens, "missing token"), res.FirstErrorLine)))
	default:
		res.Reason = sub.Reason
		res.Err = sub.Err
		res.ParsedBytes = sub.ParsedBytes
	}
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

// componentName is a file stem that names a component. `+page.svelte`,
// `[id].vue`, `404.vue` and `Foo.stories.svelte` are routes or companions, and
// naming a symbol after them would be wrong.
var componentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// componentSymbol is the symbol for the component a file defines.
func componentSymbol(path string, src []byte) (model.Symbol, bool) {
	if len(bytes.TrimSpace(src)) == 0 {
		return model.Symbol{}, false
	}
	base := filepath.Base(path)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if !componentName.MatchString(stem) {
		return model.Symbol{}, false
	}
	last := bytes.Count(src, []byte("\n")) + 1
	if src[len(src)-1] == '\n' {
		last--
	}
	return model.Symbol{
		Kind:       model.KindClass,
		Name:       stem,
		Range:      model.Range{Start: model.Position{Line: 1}, End: model.Position{Line: last}},
		Confidence: model.ConfidenceHigh,
		Source:     model.SourceSourceCode,
		Exported:   true,
	}, true
}

// snippetToken finds the ends of a Svelte 5 `{#snippet name(...)}` block.
var (
	snippetOpen  = regexp.MustCompile(`\{#snippet\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*[(<]`)
	snippetToken = regexp.MustCompile(`\{#snippet\b|\{/snippet\}`)
)

// svelteSnippets are the `{#snippet name(...)}` blocks of a Svelte 5 template.
// A snippet is a function-like definition that exists only in markup, so the
// script parser can never see it. They are found in the markup alone: script
// and style elements and comments are blanked first, so text in them that looks
// like a snippet is not one.
func svelteSnippets(src []byte, blocks [][]int) []model.Symbol {
	if !bytes.Contains(src, []byte("{#snippet")) {
		return nil
	}
	masked := markupOnly(src, blocks)

	var out []model.Symbol
	for _, m := range snippetOpen.FindAllSubmatchIndex(masked, -1) {
		startLine := bytes.Count(masked[:m[0]], []byte("\n")) + 1
		// The matching {/snippet}, counting nested snippets.
		endLine, depth := startLine, 0
		for _, t := range snippetToken.FindAllIndex(masked[m[0]:], -1) {
			if masked[m[0]+t[0]+1] == '#' {
				depth++
				continue
			}
			depth--
			if depth == 0 {
				endLine = bytes.Count(masked[:m[0]+t[1]], []byte("\n")) + 1
				break
			}
		}
		name := string(masked[m[2]:m[3]])
		out = append(out, model.Symbol{
			Kind:       model.KindFunction,
			Name:       name,
			Range:      model.Range{Start: model.Position{Line: startLine, Col: m[0] - lineStart(masked, m[0])}, End: model.Position{Line: endLine}},
			Confidence: model.ConfidenceHigh,
			Source:     model.SourceSourceCode,
			Exported:   isExported(name),
		})
	}
	return out
}

// markupOnly is a copy of a component with its script and style elements and its
// comments blanked to spaces, newlines kept: what is left is the template, with
// every byte at the offset and line it had in the file. Text in a script that
// looks like markup (a string holding `<Foo />`) is not markup.
func markupOnly(src []byte, blocks [][]int) []byte {
	masked := append([]byte(nil), src...)
	blank := func(from, to int) {
		for i := from; i < to; i++ {
			if masked[i] != '\n' {
				masked[i] = ' '
			}
		}
	}
	for _, b := range blocks {
		blank(b[0], b[1])
	}
	for _, loc := range styleBlock.FindAllIndex(masked, -1) {
		blank(loc[0], loc[1])
	}
	for pos := 0; pos < len(masked); {
		c := bytes.Index(masked[pos:], []byte("<!--"))
		if c < 0 {
			break
		}
		end := bytes.Index(masked[pos+c:], []byte("-->"))
		if end < 0 {
			blank(pos+c, len(masked))
			break
		}
		blank(pos+c, pos+c+end+3)
		pos += c + end + 3
	}
	return masked
}

// componentTag matches the opening of an element whose name is a component's:
// `<Badge`, `<Badge/>`, `<my-badge`, and Vue's `<component is=...>` is not one.
var componentTag = regexp.MustCompile(`<([A-Za-z][A-Za-z0-9]*(?:-[A-Za-z0-9]+)*)[\s/>]`)

// templateComponents are the components a template renders that the script
// imported: `<Badge />` where the script has `import Badge from './Badge.vue'`,
// or `import { Badge } from`, or `import { Chip as Badge }`. Each is returned as
// a bare call of the local name, which the resolver binds through the import
// like the script's own `Badge()` and records as an instantiation of the
// component class.
//
// Only a name the script imported is returned. A tag that no import binds is a
// global registration, a router or framework component, or an HTML element, and
// naming it here would bind it by name to whichever component of the
// repository shares it. A kebab-case tag (`<my-badge>`) is read as the
// PascalCase name Vue maps it to (`MyBadge`).
func templateComponents(src []byte, blocks [][]int, imports []Import) ([]Reference, []Import) {
	if len(imports) == 0 {
		return nil, nil
	}
	// bound maps each local name an import creates to the name the reference
	// carries. A default import of a component file is the component, whatever
	// the importer calls it: `import Pill from './Chip.vue'` makes `<Pill />` a
	// use of `Chip`, the symbol Chip.vue defines, so the reference is made to
	// that name and the resolver finds it in the file the import names.
	bound := map[string]string{}
	var extra []Import
	for _, imp := range imports {
		a := strings.TrimPrefix(imp.Alias, memberAliasPrefix)
		if a == "" {
			continue
		}
		bound[a] = a
		ext := filepath.Ext(imp.Path)
		if ext == ".vue" || ext == ".svelte" {
			stem := strings.TrimSuffix(filepath.Base(imp.Path), ext)
			if componentName.MatchString(stem) && stem != a {
				bound[a] = stem
				// The import states the component under its own name too, so the
				// resolver finds it in the file the import names.
				extra = append(extra, Import{Path: imp.Path, Alias: memberAliasPrefix + stem, Range: imp.Range})
			}
		}
	}
	if len(bound) == 0 {
		return nil, nil
	}
	masked := markupOnly(src, blocks)
	var out []Reference
	seen := map[string]bool{}
	for _, m := range componentTag.FindAllSubmatchIndex(masked, -1) {
		tag := string(masked[m[2]:m[3]])
		local := pascalCase(tag)
		name, ok := bound[local]
		if !ok {
			continue
		}
		// An uppercase first letter is how a component is told from an element
		// in a kebab-less tag: `<button>` is HTML even if a `button` is imported.
		if !strings.Contains(tag, "-") && (tag[0] < 'A' || tag[0] > 'Z') {
			continue
		}
		line := bytes.Count(masked[:m[0]], []byte("\n")) + 1
		if seen[local] {
			continue
		}
		seen[local] = true
		col := m[2] - lineStart(masked, m[2])
		out = append(out, Reference{
			Name:         name,
			Kind:         "reference.call",
			ReceiverKind: model.ReceiverBare,
			ArgCount:     model.ArityUnknown,
			Range: model.Range{
				Start: model.Position{Line: line, Col: col},
				End:   model.Position{Line: line, Col: col + len(tag)},
			},
		})
	}
	return out, extra
}

// pascalCase maps `my-badge` to `MyBadge` and leaves `Badge` as it is.
func pascalCase(tag string) string {
	if !strings.Contains(tag, "-") {
		return tag
	}
	var b strings.Builder
	for _, part := range strings.Split(tag, "-") {
		if part == "" {
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	return b.String()
}

// lineStart is the offset of the first byte of the line holding offset i.
func lineStart(b []byte, i int) int {
	return bytes.LastIndexByte(b[:i], '\n') + 1
}

// sfcDefinitions are added to the script language's own definition query for a
// component's script, and to nothing else: a .ts file is parsed exactly as it
// was. The inferred query finds functions, classes, interfaces, types, enums
// and methods; it has nothing for what makes up most of a component script,
// the state and the props.
//
//   - a top-level `const` is a constant, a top-level `let` or `var` a variable,
//     and a top-level `export let` (a Svelte prop) a field. Those bound to a
//     function stay functions (see dropShadowedBindings);
//   - the names destructured from `defineProps()` (Vue) or `$props()` (Svelte 5)
//     are the component's props, fields;
//   - `$: name = ...` is a Svelte reactive declaration, a variable;
//   - in the default-exported options object (`export default {...}`,
//     `defineComponent({...})`, `Vue.extend({...})`, `module.exports = {...}`),
//     a property whose value is a function is a method, directly in the object
//     and one object down (`methods: { go: function () {} }`).
//
// Not captured: declarations inside a function body, destructured bindings other
// than props, and function-valued properties in any other object.
var sfcDefinitions = sfcStatic + "\n" + sfcOptionsObject

const sfcStatic = `
(program (lexical_declaration (variable_declarator name: (identifier) @name) @definition.constant) @_d (#match? @_d "^const\\b"))
(program (export_statement declaration: (lexical_declaration (variable_declarator name: (identifier) @name) @definition.constant) @_d (#match? @_d "^const\\b")))
(program (lexical_declaration (variable_declarator name: (identifier) @name) @definition.variable) @_d (#match? @_d "^let\\b"))
(program (export_statement declaration: (lexical_declaration (variable_declarator name: (identifier) @name) @definition.field) @_d (#match? @_d "^let\\b")))
(program (variable_declaration (variable_declarator name: (identifier) @name) @definition.variable))
(program (export_statement declaration: (variable_declaration (variable_declarator name: (identifier) @name) @definition.variable)))

(program (lexical_declaration (variable_declarator name: (object_pattern (shorthand_property_identifier_pattern) @name @definition.field) value: (call_expression function: (identifier) @_f)) (#any-of? @_f "defineProps" "$props")))
(program (lexical_declaration (variable_declarator name: (object_pattern (object_assignment_pattern left: (shorthand_property_identifier_pattern) @name) @definition.field) value: (call_expression function: (identifier) @_f)) (#any-of? @_f "defineProps" "$props")))
(program (lexical_declaration (variable_declarator name: (object_pattern (pair_pattern value: (identifier) @name) @definition.field) value: (call_expression function: (identifier) @_f)) (#any-of? @_f "defineProps" "$props")))

(program (lexical_declaration (variable_declarator name: (object_pattern (pair_pattern value: (assignment_pattern left: (identifier) @name)) @definition.field) value: (call_expression function: (identifier) @_f)) (#any-of? @_f "defineProps" "$props")))

(program (labeled_statement label: (statement_identifier) @_l body: (expression_statement (assignment_expression left: (identifier) @name))) @definition.variable (#eq? @_l "$"))
`

// sfcOptionsObject builds the patterns for function-valued properties of a
// component options object, for each place such an object is written.
var sfcOptionsObject = func() string {
	fn := `[(function_expression) (arrow_function)]`
	objects := []string{
		`(object (pair key: (property_identifier) @name value: ` + fn + `) @definition.method)`,
		`(object (pair value: (object (pair key: (property_identifier) @name value: ` + fn + `) @definition.method)))`,
	}
	var out []string
	for _, o := range objects {
		out = append(out,
			`(export_statement value: `+o+`)`,
			`(export_statement value: (call_expression arguments: (arguments `+o+`)))`,
			`(program (expression_statement (assignment_expression left: (member_expression object: (identifier) @_m property: (property_identifier) @_p) right: `+o+`)) (#eq? @_m "module") (#eq? @_p "exports"))`,
		)
	}
	return strings.Join(out, "\n")
}()

// sfcDelegate is the parser for the language of a script block: the registry's
// parser for that language, with sfcDefinitions added to its query. It is kept
// beside the registry's own parsers, so it is drained with them, and it is
// built only when a component is parsed. If the extra query is not valid for
// the grammar the plain parser is used and the component loses only what the
// extra patterns add.
func (r *registry) sfcDelegate(ext string) *genericParser {
	plain, ok := r.ForFile("sfc." + ext).(*genericParser)
	if !ok || plain == nil {
		return nil
	}
	key := "sfc:" + plain.entry.Name

	r.mu.RLock()
	d, ok := r.parsers[key]
	r.mu.RUnlock()
	if ok {
		return d
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if d, ok := r.parsers[key]; ok {
		return d
	}
	d = plain
	if plain.initErr == nil && plain.tagsQ != nil {
		if built := newGenericParser(plain.entry, plain.cfg); built.initErr == nil {
			query := built.tagsQuery + "\n" + hostedDefinitions(sfcDefinitions)
			if q, err := ts.NewQuery(query, built.lang); err == nil {
				built.tagsQuery, built.tagsQ = query, q
				d = built
			}
		}
	}
	r.parsers[key] = d
	return d
}

// hostedTagSuffix marks a definition captured by a pattern a host adds to a
// script language's query. A component's script is JavaScript or TypeScript
// with different rules about what is a definition: a top-level `const count =
// ref(0)` is component state, where in a plain module a constant nobody
// exports is not indexed. The script language's refiner (lang_javascript.go)
// enforces the plain-module rules by position, so without the mark it dropped
// or re-labelled every symbol the component patterns exist to add.
const hostedTagSuffix = ".hosted"

var definitionCapture = regexp.MustCompile(`@definition\.[a-z_]+`)

// unmarkHostedTags removes the mark from every tag that carries it and reports
// which tags did, by position. It runs before anything else reads a tag's
// kind, so the rest of the parser sees ordinary kinds and only the step that
// applies the script language's refinements needs to know.
func unmarkHostedTags(tags []ts.Tag) map[int]bool {
	var hosted map[int]bool
	for i := range tags {
		if !strings.HasSuffix(tags[i].Kind, hostedTagSuffix) {
			continue
		}
		tags[i].Kind = strings.TrimSuffix(tags[i].Kind, hostedTagSuffix)
		if hosted == nil {
			hosted = make(map[int]bool, 8)
		}
		hosted[i] = true
	}
	return hosted
}

// hostedDefinitions marks every definition capture in a query as the host's.
func hostedDefinitions(query string) string {
	return definitionCapture.ReplaceAllStringFunc(query, func(c string) string { return c + hostedTagSuffix })
}

// bindingRank orders the kinds two patterns can give one declaration, most
// specific first: what a component says about a name outranks what the script
// language says about the same statement.
func bindingRank(k model.EntityKind) int {
	switch k {
	case model.KindFunction, model.KindMethod, model.KindClass, model.KindInterface, model.KindTypeAlias:
		return 0
	case model.KindField:
		return 1
	case model.KindConstant:
		return 2
	case model.KindVariable:
		return 3
	}
	return 4
}

// dropShadowedBindings removes a constant or variable that is the same
// declaration as a function: `const f = () => {}` is a function, and the
// pattern for top-level constants matches it too. The two are different nodes,
// so deduplication by position does not see them as one.
func dropShadowedBindings(in []model.Symbol) []model.Symbol {
	type site struct {
		name string
		line int
	}
	callable := map[site]bool{}
	for _, s := range in {
		if s.Kind == model.KindFunction || s.Kind == model.KindMethod || s.Kind == model.KindClass {
			callable[site{s.Name, s.Range.Start.Line}] = true
		}
	}
	// The script language's own patterns and the component's can both match
	// one declaration, with the same kind or with different ones (`export let
	// name` is an exported variable to JavaScript and a prop to Svelte). One
	// symbol is kept per name and line: the most specific kind, first seen.
	best := map[site]int{}
	for _, s := range in {
		k := site{s.Name, s.Range.Start.Line}
		if r, seen := best[k]; !seen || bindingRank(s.Kind) < r {
			best[k] = bindingRank(s.Kind)
		}
	}
	kept := map[site]bool{}
	out := in[:0]
	for _, s := range in {
		k := site{s.Name, s.Range.Start.Line}
		if (s.Kind == model.KindConstant || s.Kind == model.KindVariable || s.Kind == model.KindField) && callable[k] {
			continue
		}
		if bindingRank(s.Kind) != best[k] || kept[k] {
			continue
		}
		kept[k] = true
		out = append(out, s)
	}
	return out
}

// interface assertion: sfcParser is a Parser.
var _ Parser = (*sfcParser)(nil)
