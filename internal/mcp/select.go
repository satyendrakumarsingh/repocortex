package mcp

// Turning what a caller typed into exactly one definition, or saying why not.
//
// Every tool that answers about a symbol starts here. There used to be three
// ways of doing it and each was wrong in its own direction:
//
//   - find_callers, find_references and plan_rename fetched the first FIVE
//     definitions of the name and looked among those for the path hint. With
//     fifteen definitions named New, a path naming the eleventh matched
//     nothing, and the first of the five was answered for with no note.
//   - find_callees, find_implementations and trace_call_path did the same and
//     ignored kind, so "what implements Render" was answered for a method
//     called Render instead of the interface.
//   - analyze_impact, find_tests, plan_change and find_owners refused an
//     ambiguous name, which is right, but reported the refusal as
//     error_code: internal and told the caller to re-ask with a qualified name
//     that every overload shares.
//
// One rule replaces them. A name selects a definition only when it leaves
// exactly one after the caller's `path` and `line` are applied and, where the
// tool is about one kind of thing, after that kind is preferred. Otherwise
// nothing is answered and the candidates are listed with the arguments that
// select each. RepoCortex never picks one of several and answers for it.

import (
	"context"
	"fmt"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

// lineArg is the `line` selector every symbol tool offers beside `path`.
//
// EMBEDDED RATHER THAN REPEATED, for the reason minConfidenceArg is: the
// description is what an agent reads before deciding what to pass, and a copy
// per tool would drift.
//
// It exists because a path is not always enough. Overloads live in one file
// under one qualified name, so the only thing that tells the third `fromJson`
// from the fourth is where it starts.
type lineArg struct {
	Line int `json:"line,omitempty" jsonschema:"Line the definition starts on, to choose between several in one file (overloads)."`
}

// pageArg is the `limit` and `offset` every list tool offers.
type pageArg struct {
	Limit  int `json:"limit,omitempty" jsonschema:"Most rows to return. Default 50, maximum 200."`
	Offset int `json:"offset,omitempty" jsonschema:"Rows to skip. The answer names the next offset when more rows exist."`
}

// page validates the wire values and turns them into a Page with no total yet.
//
// A negative value is an error and not a silent default: a caller who wrote
// limit=-1 has a bug, and answering the default question would hide it. A
// limit above the maximum is clamped, and the answer says what the maximum is.
func (s *Server) page(a pageArg, noun string) (*Page, error) {
	if a.Limit < 0 || a.Offset < 0 {
		return nil, fmt.Errorf("limit and offset must not be negative (got limit=%d offset=%d)", a.Limit, a.Offset)
	}
	limit := a.Limit
	if limit == 0 {
		limit = s.limits.MaxItems
	}
	limit = min(limit, s.maxPage())
	return &Page{Offset: a.Offset, Limit: limit, noun: noun, max: s.maxPage()}, nil
}

// maxPage is the most rows one answer may carry.
func (s *Server) maxPage() int {
	return max(s.limits.MaxPageItems, s.limits.MaxItems)
}

// symbolQuery is one request to select a definition.
type symbolQuery struct {
	// Within narrows a relationship answer to call sites under a path (see
	// registry.EdgeFilter.Within). Not used to choose the symbol.
	Within string
	// Tool and the three parameter names are what the disambiguation answer
	// prints, so the example it gives can be pasted back unchanged.
	Tool      string
	Param     string
	PathParam string
	LineParam string

	Name string
	Path string
	Line int

	// Prefer lists the kinds the tool is about. When several candidates remain
	// and only some are of these kinds, the others are set aside. PreferWord
	// names them in the note ("a type").
	Prefer     []string
	PreferWord string

	// PreferNoun is one such thing, singular ("type"), for the sentence that
	// says there is none. Tolerate lists kinds that are still answered for when
	// no preferred kind exists, because the question has a meaning for them:
	// a class has callers, in the sense of what constructs it.
	PreferNoun string
	Tolerate   []string

	// Admit is asked about a definition of another kind when no preferred one
	// exists, and answers whether the question still has a meaning for it: an
	// abstract method has implementations though it is not a type. AdmitWhy
	// completes "none of them ..." in the sentence that says none does.
	Admit    func(ctx context.Context, runID int64, c registry.SymbolCandidate) bool
	AdmitWhy string

	// Rest is the caller's other arguments, as they should appear after the
	// selector in the example call.
	Rest string

	// narrowed is set by selectSymbol: the selectors that were applied and
	// still left several definitions.
	narrowed string
}

// selection is a query that came down to one definition.
type selection struct {
	RunID  int64
	Status string
	Target registry.SymbolCandidate

	// Total is how many definitions share the name. How says what narrowed
	// them to one; empty when the name was unique.
	Total int
	How   string
}

// note says which definition was answered for when the name alone did not
// decide it. Empty for a unique name, where there is nothing to say.
func (sel selection) note(q symbolQuery) string {
	if sel.Total <= 1 {
		return ""
	}
	return fmt.Sprintf("%d definitions are named %q. This answer is for the %s at %s:%d, chosen %s. "+
		"Pass %s and %s to choose another.",
		sel.Total, sel.Target.Name, sel.Target.Kind, sel.Target.FilePath, sel.Target.StartLine,
		sel.How, q.PathParam, q.LineParam)
}

const (
	// maxSelectRows is how many same-named definitions are read to choose
	// among. A name with more than this is narrowed in the store by path
	// instead of in memory.
	maxSelectRows = 500

	// maxCandidateRows is how many candidates a disambiguation answer lists.
	// Each costs a row and a line of source, and the answer must stay well
	// inside the response size limit.
	maxCandidateRows = 25

	// candidateExcerptBytes bounds the declaration line shown per candidate.
	candidateExcerptBytes = 200
)

// Kinds a tool can prefer.
var (
	typeKinds     = []string{"interface", "class", "type_alias"}
	callableKinds = []string{"function", "method"}
)

// outcomeAmbiguous is Response.Outcome for a disambiguation answer.
const outcomeAmbiguous = "ambiguous_name"

// outcomeNoSuchKind is Response.Outcome for an answer that found definitions of
// the name and none of the kind the tool is about.
const outcomeNoSuchKind = "kind_not_indexed"

// selectSymbol turns q into one definition.
//
// On any outcome other than success it returns the finished result the caller
// must hand back unchanged: a handler that also returned an error would have
// its guidance replaced by the SDK's bare protocol error (see failWith).
func (s *Server) selectSymbol(ctx context.Context, req *sdk.CallToolRequest, q symbolQuery) (selection, *sdk.CallToolResult, *Response) {
	if q.Line < 0 {
		sc, r, _ := failWith("invalid_argument",
			fmt.Sprintf("Pass %s as the line a definition starts on, or omit it.", q.LineParam),
			"%s must not be negative", q.LineParam)
		return selection{}, sc, r
	}
	runID, status, err := s.currentRun(ctx)
	if err != nil {
		sc, r, _ := s.noRun(ctx, req, err)
		return selection{}, sc, r
	}

	name := strings.TrimSpace(q.Name)
	wantPath := cleanPathHint(q.Path)

	all, total, err := s.deps.Registry.SymbolCandidates(ctx, runID, name, maxSelectRows)
	if err != nil {
		sc, r, _ := failFrom(err, "symbol lookup failed: %v", err)
		return selection{}, sc, r
	}
	if total == 0 {
		// Not a name: perhaps the qualified name a previous answer printed.
		// Several rows can share one, so it is a candidate set like any other.
		all, total, err = s.deps.Registry.SymbolCandidatesByFQN(ctx, runID, name, maxSelectRows)
		if err != nil {
			sc, r, _ := failFrom(err, "symbol lookup failed: %v", err)
			return selection{}, sc, r
		}
	}
	// Not a qualified name either: `Guard.AgainstNull`, `Guard::AgainstNull` and
	// `Mailer#deliver` name a member by its owner. Tried last, so a definition
	// whose own name contains a dot is still found by that name first.
	qualified := false
	if total == 0 {
		all, total, err = s.deps.Registry.SymbolCandidatesQualified(ctx, runID, name, maxSelectRows)
		if err != nil {
			sc, r, _ := failFrom(err, "symbol lookup failed: %v", err)
			return selection{}, sc, r
		}
		qualified = total > 0
	}
	if total == 0 {
		sc, r, _ := s.missingSymbol(ctx, req, runID, status, name)
		return selection{}, sc, r
	}

	fail := func(reason string, list []registry.SymbolCandidate) (selection, *sdk.CallToolResult, *Response) {
		sc, r := s.disambiguate(ctx, req, runID, status, q, reason, list, total)
		return selection{}, sc, r
	}
	// narrowed says which of the caller's selectors were applied, for the
	// sentence that says they were not enough.
	var narrowed []string

	cands := all
	var how []string

	if wantPath != "" {
		// The name is too common to have read every definition of it, so the
		// path is applied where all of them are: in the store.
		if total > len(all) && !qualified {
			in, _, ierr := s.deps.Registry.SymbolCandidatesIn(ctx, runID, name, wantPath, maxSelectRows)
			if ierr != nil {
				sc, r, _ := failFrom(ierr, "symbol lookup failed: %v", ierr)
				return selection{}, sc, r
			}
			cands = in
		}
		cands = matchPath(cands, wantPath)
		if len(cands) == 0 {
			return fail(fmt.Sprintf("no definition of %q is in a file matching %s=%q", name, q.PathParam, q.Path), all)
		}
		how = append(how, fmt.Sprintf("by %s", q.PathParam))
		narrowed = append(narrowed, fmt.Sprintf("%s=%s", q.PathParam, quoteArg(q.Path)))
	}

	if q.Line > 0 {
		at := matchLine(cands, q.Line)
		if len(at) == 0 {
			where := ""
			if wantPath != "" {
				where = fmt.Sprintf(" in a file matching %s=%q", q.PathParam, q.Path)
			}
			return fail(fmt.Sprintf("no definition of %q starts at or contains line %d%s", name, q.Line, where), cands)
		}
		cands = at
		how = append(how, fmt.Sprintf("by %s", q.LineParam))
		narrowed = append(narrowed, fmt.Sprintf("%s=%d", q.LineParam, q.Line))
	}

	// A tool about one kind of thing, and no definition of that kind: the
	// answer would be "0" about something the question is not about. A path or
	// a line is the caller choosing, and is honoured.
	if len(how) == 0 && len(q.Prefer) > 0 && q.PreferNoun != "" &&
		len(ofKinds(cands, q.Prefer)) == 0 && len(ofKinds(cands, q.Tolerate)) == 0 && !admitsAny(ctx, runID, q, cands) {
		sc, r := s.noSuchKind(ctx, req, runID, status, q, cands, total)
		return selection{}, sc, r
	}

	// Kind is a preference, applied last and only to break a tie the caller's
	// own selectors left: it may set candidates aside, never override a path
	// or a line the caller gave.
	if len(cands) > 1 && len(q.Prefer) > 0 {
		if preferred := ofKinds(cands, q.Prefer); len(preferred) > 0 && len(preferred) < len(cands) {
			cands = preferred
			how = append(how, fmt.Sprintf("because %s is about %s and it is the only one that is",
				q.Tool, q.PreferWord))
		}
	}

	if len(cands) != 1 {
		q.narrowed = strings.Join(narrowed, " ")
		return fail("", cands)
	}
	if len(how) == 0 {
		how = []string{"because it is the only one"}
	}
	return selection{
		RunID: runID, Status: status, Target: cands[0],
		Total: total, How: strings.Join(how, " and "),
	}, nil, nil
}

// cleanPathHint normalises a path a caller typed: surrounding space, a leading
// "./" and Windows separators mean nothing to the index, which stores
// repository-relative paths with forward slashes.
func cleanPathHint(p string) string {
	p = strings.TrimSpace(p)
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimPrefix(p, "./")
	return p
}

// matchPath narrows candidates to the files a path hint names.
//
// Three readings, strictest first, and the first that matches anything wins:
// the exact file; a whole path segment or run of segments (a directory, or a
// file name with its parent directories); any substring. Strictest first is
// what "honour the path" means: `internal/web/web.go` must select that file
// even when `vendor/x/internal/web/web.go` also defines the name, and it used
// to select whichever sorted first.
func matchPath(cands []registry.SymbolCandidate, want string) []registry.SymbolCandidate {
	trimmed := strings.Trim(want, "/")
	tiers := []func(string) bool{
		func(p string) bool { return p == want || p == trimmed },
		func(p string) bool { return strings.Contains("/"+p+"/", "/"+trimmed+"/") },
		func(p string) bool { return strings.Contains(p, want) },
	}
	for _, match := range tiers {
		var out []registry.SymbolCandidate
		for _, c := range cands {
			if match(c.FilePath) {
				out = append(out, c)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// matchLine narrows candidates to the definition a line names: the one that
// starts on it, or failing that the innermost one that contains it.
func matchLine(cands []registry.SymbolCandidate, line int) []registry.SymbolCandidate {
	var starts []registry.SymbolCandidate
	for _, c := range cands {
		if c.StartLine == line {
			starts = append(starts, c)
		}
	}
	if len(starts) > 0 {
		return starts
	}

	var inside []registry.SymbolCandidate
	best := -1
	for _, c := range cands {
		if c.StartLine > line || c.EndLine < line {
			continue
		}
		span := c.EndLine - c.StartLine
		switch {
		case best < 0 || span < best:
			best, inside = span, []registry.SymbolCandidate{c}
		case span == best:
			inside = append(inside, c)
		}
	}
	return inside
}

func ofKinds(cands []registry.SymbolCandidate, kinds []string) []registry.SymbolCandidate {
	var out []registry.SymbolCandidate
	for _, c := range cands {
		for _, k := range kinds {
			if c.Kind == k {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// describeCandidate says what a definition is in the terms that tell it from
// its namesakes: its kind, the type that owns it, how many parameters it
// declares and the lines it spans. Only what the index recorded is printed.
func describeCandidate(c registry.SymbolCandidate) string {
	parts := []string{c.Kind}
	if c.Owner != "" {
		parts = append(parts, fmt.Sprintf("in %s %s", c.OwnerKind, c.Owner))
	}
	if c.ParamCount.Known() {
		parts = append(parts, fmt.Sprintf("%d parameter(s)", c.ParamCount.Count()))
	}
	if c.EndLine > c.StartLine {
		parts = append(parts, fmt.Sprintf("lines %d-%d", c.StartLine, c.EndLine))
	}
	return strings.Join(parts, ", ")
}

// selectorArgs renders the arguments that select c, in the form the CLI and an
// MCP call both accept.
func selectorArgs(q symbolQuery, c registry.SymbolCandidate) string {
	return fmt.Sprintf("%s=%s %s=%d", q.PathParam, quoteArg(c.FilePath), q.LineParam, c.StartLine)
}

// quoteArg quotes a value only when it needs it, so the common path prints as
// a path.
func quoteArg(v string) string {
	if strings.ContainsAny(v, " \t'\"") {
		return fmt.Sprintf("%q", v)
	}
	return v
}

// admitsAny reports whether any of the first few candidates is one the query
// still has a meaning for.
func admitsAny(ctx context.Context, runID int64, q symbolQuery, cands []registry.SymbolCandidate) bool {
	if q.Admit == nil {
		return false
	}
	for i, c := range cands {
		if i == maxCandidateRows {
			break
		}
		if q.Admit(ctx, runID, c) {
			return true
		}
	}
	return false
}

// noSuchKind is the answer to a name whose only definitions are of another kind
// than the one the tool is about.
//
// It exists because the name used to select what it found. benchmark's
// BenchmarkReporter class was not indexed, a constructor named like it was, and
// find_implementations answered "0 implementing" for the constructor: a zero
// about a method, read as a zero about the type. Nothing is analysed, and the
// first line says what was asked, what exists instead, and that it is not zero.
//
// No rows: the definitions are in the sentence, because a list of rows under a
// tool's name reads as its result and a client that offers rows to choose from
// would re-ask for the very definition this refused.
func (s *Server) noSuchKind(ctx context.Context, req *sdk.CallToolRequest, runID int64, status string,
	q symbolQuery, cands []registry.SymbolCandidate, total int) (*sdk.CallToolResult, *Response) {
	name := strings.TrimSpace(q.Name)

	shown := cands
	if len(shown) > 3 {
		shown = shown[:3]
	}
	var found []string
	for _, c := range shown {
		found = append(found, fmt.Sprintf("a %s at %s:%d%s", c.Kind, c.FilePath, c.StartLine, inOwner(c)))
	}
	exists := strings.Join(found, "; ")
	if more := max(total, len(cands)) - len(shown); more > 0 {
		exists += fmt.Sprintf("; and %d more", more)
	}

	resp := &Response{
		IndexRevision: runID,
		IndexStatus:   status,
		Outcome:       outcomeNoSuchKind,
		Summary: fmt.Sprintf("%s — no %s named %q is indexed, so %s answered for nothing. "+
			"The only definition(s) of that name: %s%s.", leadUnknown, q.PreferNoun, name, q.Tool, exists, admitWhy(q)),
	}
	resp.Notes = append(resp.Notes,
		fmt.Sprintf("%s is about %s. This is not a count of zero: nothing of that kind was looked at. If the %s "+
			"exists in the repository, the index does not hold it (a file that did not parse in full, a "+
			"declaration behind a macro, generated code): run search_code for %q to see where it is written.",
			q.Tool, q.PreferWord, q.PreferNoun, name),
		fmt.Sprintf("To answer for what is indexed anyway, pass %s and %s of the definition above.",
			q.PathParam, q.LineParam))

	sc, r, _ := s.reply(ctx, req, resp)
	return sc, r
}

func admitWhy(q symbolQuery) string {
	if q.AdmitWhy == "" {
		return ""
	}
	return ", and none " + q.AdmitWhy
}

// inOwner says which type holds a member, when the index recorded one.
func inOwner(c registry.SymbolCandidate) string {
	if c.Owner == "" {
		return ""
	}
	return fmt.Sprintf(" (in %s %s)", c.OwnerKind, c.Owner)
}

// disambiguate is the answer to a name that did not come down to one
// definition.
//
// IT IS AN ANSWER, NOT AN ERROR. An ambiguous name is a normal outcome of
// asking by name, and the agent can fix it by asking a better question, which
// is exactly the condition failWith's policy says must not be flagged isError:
// a model that sees a tool fail stops calling it. It used to surface as
// error_code: internal with the hint "RepoCortex failed internally", and exit
// status 1 from the CLI.
//
// Nothing is analysed. The first line says so, because a list of definitions
// under an analyze_impact call could otherwise be read as its result.
func (s *Server) disambiguate(ctx context.Context, req *sdk.CallToolRequest, runID int64, status string,
	q symbolQuery, reason string, cands []registry.SymbolCandidate, total int) (*sdk.CallToolResult, *Response) {
	name := strings.TrimSpace(q.Name)

	// One definition exists and the selector missed it: that is not an
	// ambiguity, there is nothing to choose between. Say where it is.
	if reason != "" && total == 1 && len(cands) == 1 {
		c := cands[0]
		resp := &Response{
			IndexRevision: runID,
			IndexStatus:   status,
			Outcome:       outcomeNoSuchKind,
			Summary: fmt.Sprintf("%s — %s, so %s answered for nothing. The only definition of %q is a %s at %s:%d%s; "+
				"pass that %s and %s, or leave them out.",
				leadUnknown, reason, q.Tool, name, c.Kind, c.FilePath, c.StartLine, inOwner(c), q.PathParam, q.LineParam),
		}
		sc, r, _ := s.reply(ctx, req, resp)
		return sc, r
	}

	// matched is how many definitions the caller's own selectors left. With no
	// selector it is every definition of the name, which is the counted total
	// and not the number of rows read.
	matched := len(cands)
	var summary string
	switch {
	case reason != "":
		summary = fmt.Sprintf("%s — %s, so %s answered for nothing. %d definition(s) have that name; "+
			"pass the %s and %s of one row below.",
			leadAmbiguous, reason, q.Tool, total, q.PathParam, q.LineParam)
	case q.narrowed != "":
		summary = fmt.Sprintf("%s — %d definitions of %q match %s and %s answered for none of them. "+
			"Pass the %s and %s of one row below.",
			leadAmbiguous, matched, name, q.narrowed, q.Tool, q.PathParam, q.LineParam)
	default:
		matched = max(total, len(cands))
		summary = fmt.Sprintf("%s — %q names %d definitions and %s answered for none of them. "+
			"Pass the %s and %s of one row below.",
			leadAmbiguous, name, matched, q.Tool, q.PathParam, q.LineParam)
	}

	resp := &Response{
		IndexRevision: runID,
		IndexStatus:   status,
		Summary:       summary,
		Outcome:       outcomeAmbiguous,
	}

	shown := cands
	if len(shown) > maxCandidateRows {
		shown = shown[:maxCandidateRows]
	}
	for _, c := range shown {
		resp.Items = append(resp.Items, Item{
			Kind: "candidate", Name: c.Name, FQN: c.FQN, Path: c.FilePath, Line: c.StartLine,
			// No Confidence: these are definitions to choose between, not
			// relationships, and a tier here would be graded into a verdict
			// about an analysis that did not run.
			Why:     describeCandidate(c) + "; select with " + selectorArgs(q, c),
			Excerpt: s.declarationLine(ctx, runID, c),
		})
	}

	if len(shown) > 0 {
		example := fmt.Sprintf("%s %s=%s %s%s", q.Tool, q.Param, quoteArg(name), selectorArgs(q, shown[0]), q.Rest)
		resp.Notes = append(resp.Notes,
			"Each row is one definition. Repeat the call with the row's path and line, for example: "+example)
	}
	if reason != "" {
		matched = total
	}
	if len(shown) < matched {
		resp.Notes = append(resp.Notes, fmt.Sprintf(
			"%d definition(s) are listed of %d that match. Pass %s (a file or a directory) to narrow the list.",
			len(shown), matched, q.PathParam))
	}
	resp.Notes = append(resp.Notes,
		"A qualified name does not select one: it is the file and the identifier, so overloads and "+
			"same-named methods in one file share it. The path and line always do.")

	sc, r, _ := s.reply(ctx, req, resp)
	return sc, r
}

// declarationLine is the first line of a definition, read from the index, for
// a reader choosing between namesakes. It is repository content and is served
// as an excerpt, under SOURCE EVIDENCE, never as metadata.
//
// A read that fails returns "": the row still names the file and line.
//
// The first line that NAMES the symbol, within the first few. A definition's
// recorded start is often an annotation or a decorator, and
// `@SuppressWarnings("unchecked")` printed five times tells five overloads
// apart no better than their shared name did.
func (s *Server) declarationLine(ctx context.Context, runID int64, c registry.SymbolCandidate) string {
	if c.StartLine <= 0 {
		return ""
	}
	text, err := s.deps.Registry.FileExcerpt(ctx, runID, c.FilePath, c.StartLine, declarationScanLines)
	if err != nil {
		return ""
	}
	lines := strings.Split(text, "\n")
	pick := strings.TrimSpace(lines[0])
	for _, line := range lines {
		if strings.Contains(line, c.Name) {
			pick = strings.TrimSpace(line)
			break
		}
	}
	return clip(pick, candidateExcerptBytes)
}

// declarationScanLines is how far past a definition's first line
// declarationLine looks for the line that names it.
const declarationScanLines = 6
