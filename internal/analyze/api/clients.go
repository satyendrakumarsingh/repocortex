package api

// Outbound HTTP calls: the counterpart to the routes above.
//
// # Why this belongs next to route extraction
//
// A route says what a service ANSWERS. It does not say who ASKS. Without the
// asking side the graph stops at the endpoint: `endpoint -> handler -> service
// -> table` is traceable, but "which screen breaks if this handler changes" is
// not, because nothing records that a component calls the endpoint. This finds
// the other end of that hop.
//
// # Why it is client-specific, for exactly the reason routes are framework-specific
//
// There is no language-level concept of "an outbound HTTP call" either. It is
// `fetch` in a browser, `axios.post` in a React app, `http.NewRequest` in Go —
// each its own spelling, each needing to be known. The same gate applies and is
// the whole defence against noise: every pattern here requires the library's own
// spelling. `fetch(` and `axios.get(` are matched; `api.get(`, `refetch(` and
// `cache.Get(` are not, because a bare `.Get(` on an arbitrary receiver matches
// every cache, map and repository in a Go codebase, and an invented call is
// worse than a missing one for the same reason an invented endpoint is: it
// appears in an impact report as a dependency that does not exist.
//
// # What is covered
//
// fetch and axios (JavaScript/TypeScript), net/http (Go): http.NewRequest,
// http.NewRequestWithContext, and Get/Post/PostForm on the http package or on a
// receiver that names itself a client. SupportedClients states it so `doctor`
// can print it.
//
// NOT covered, and deliberately: Java (RestTemplate, WebClient, OkHttp),
// Python (requests, httpx), generated API clients of any kind, and any axios
// instance renamed by axios.create(). Each is a separate body of work and half
// of one would be the overclaim this package exists to avoid.
//
// # Paths that cannot be resolved, and why they are still recorded
//
// A path written as a template literal, an f-string or a concatenation is not
// knowable from the text: `` `/api/users/${id}` `` is a different URL on every
// call. The rule is the one route extraction already follows — report the
// literal part, never invent the rest — with one difference that the direction
// forces. A route whose path is empty is dropped, because a route with no path
// cannot exist. A CALL whose path is entirely dynamic does exist; it is a real
// outbound request whose target is unknown, and dropping it would silently
// shrink the denominator in "how many of this component's calls did we
// resolve". So it is kept, with Dynamic set and no path invented, and a
// consumer that needs a resolved target must check Dynamic rather than trusting
// a Path that happens to be non-empty.

import (
	"regexp"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Client identifies which calling convention matched, so a report can say how a
// call was found rather than presenting all calls as equally certain.
type Client string

const (
	ClientFetch     Client = "fetch"
	ClientAxios     Client = "axios"
	ClientGoNetHTTP Client = "go-net-http"
)

// SupportedClients is the coverage statement for outbound calls, and is read
// the same way SupportedFrameworks is: "no calls found" means either that this
// file makes none, or that it uses a client not in this list, and a user must be
// able to tell those apart.
func SupportedClients() []Client {
	return []Client{ClientFetch, ClientAxios, ClientGoNetHTTP}
}

// Call is one discovered outbound HTTP request.
type Call struct {
	// Method is the HTTP verb, uppercased, where the syntax states it. Empty
	// means the call states no verb this scan could read — a Go request built
	// from a variable, or a fetch whose options object was not closed within
	// the lines examined. It is reported as "ANY" rather than guessed at.
	Method string
	// Path is the LITERAL part of the target, as written. It is not normalised
	// and not completed: for a dynamic target it holds the constant prefix and
	// nothing more, and it is empty when nothing at all was literal.
	Path string
	// Caller is the name of the declaration whose body contains the call. Empty
	// when the call sits outside every declaration this scan could see.
	//
	// It is a bare name, not an FQN, because this package has no symbol table.
	// See attributeCalls: a caller that HAS one should prefer it.
	Caller string
	// Client is the convention that matched.
	Client Client
	// Line is the 1-based line the call was written on.
	Line int
	// Confidence reflects how certain the extraction is, never whether the call
	// succeeds at run time.
	Confidence model.Confidence
	// Dynamic marks a target assembled rather than literal. Path then holds the
	// literal prefix, which may be empty.
	Dynamic bool
}

// Signature renders a call the way a person writes it in a ticket.
//
// A dynamic target is spelled out rather than smoothed over: `GET /api/users/`
// would read as a call to a path that is never actually requested, so the
// unresolved remainder is named.
func (c Call) Signature() string {
	m := c.Method
	if m == "" {
		m = "ANY"
	}
	p := c.Path
	if c.Dynamic {
		p += "<unresolved>"
	}
	return m + " " + p
}

// ExtractCalls finds every outbound HTTP call in one file.
//
// Same trade as Extract, and the same reason: the constructs are line-local and
// syntactically distinctive, so a scan gets them right without a per-language
// query set. Where a call is spread across lines, the parts on later lines are
// not read, and the result is an unresolved path rather than a wrong one.
func ExtractCalls(path string, src []byte) []Call {
	lang := languageOf(path)
	if lang != "go" && lang != "js" {
		// Java and Python declare routes here but their clients are not covered.
		// Returning nothing is the honest answer; SupportedClients says so.
		return nil
	}
	lines := strings.Split(string(src), "\n")

	var calls []Call
	if lang == "go" {
		calls = extractGoClient(lines)
	} else {
		calls = extractJSClient(lines)
	}
	return withTarget(attributeCalls(lang, lines, calls))
}

// withTarget is the exit filter, and it is deliberately weaker than withPath.
//
// `fetch("")` names no target and cannot be a request anyone meant to make, so
// it is dropped exactly as an empty route is. `fetch(url)` names no target
// either, but it IS a request — the difference is that the author wrote
// something we could not read, rather than nothing. Dropping the second would
// make an unresolved call indistinguishable from no call at all, which is the
// one thing a coverage number must not do.
func withTarget(calls []Call) []Call {
	kept := calls[:0]
	for _, c := range calls {
		if c.Path == "" && !c.Dynamic {
			continue
		}
		kept = append(kept, c)
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// ---------------------------------------------------------------- JavaScript

var (
	// fetch("/api/users") — and `await fetch(`, `return fetch(`, `window.fetch(`.
	// The leading group is what stops `refetch(` and `client.fetch(` matching:
	// Go's regexp has no lookbehind, so the preceding character is captured and
	// required to be a non-identifier.
	jsFetchCall = regexp.MustCompile(`(^|[^\w$.])(?:(?:window|globalThis|self)\.)?fetch\s*\(`)
	// axios.get("/api/users"). The receiver must be spelled `axios`; an instance
	// from axios.create() assigned to `api` is not matched, deliberately.
	jsAxiosCall = regexp.MustCompile(`(^|[^\w$.])axios\s*\.\s*(get|post|put|patch|delete)\s*\(`)
	// method: "POST" inside a fetch options object.
	jsMethodOption = regexp.MustCompile("(?i)\\bmethod\\s*:\\s*['\"`]([A-Za-z]+)['\"`]")
)

func extractJSClient(lines []string) []Call {
	var out []Call
	for i, line := range lines {
		if isComment(line) {
			continue
		}

		if loc := jsAxiosCall.FindStringSubmatchIndex(line); loc != nil && !inString(line, loc[3]) {
			verb := strings.ToUpper(line[loc[4]:loc[5]])
			if httpMethods[verb] {
				lit, dyn := literalArg(line[loc[1]:])
				out = append(out, newCall(verb, lit, dyn, ClientAxios, i+1))
			}
			continue
		}

		loc := jsFetchCall.FindStringSubmatchIndex(line)
		if loc == nil || inString(line, loc[3]) {
			continue
		}
		rest := line[loc[1]:]
		lit, dyn := literalArg(rest)
		method, known := fetchMethod(lines, i, rest)
		c := newCall(method, lit, dyn, ClientFetch, i+1)
		if !known {
			// The options object states a verb somewhere past the window that was
			// read. GET is fetch's default and would usually be right, which is
			// precisely why claiming it here would be a guess dressed as a fact.
			c.Confidence = model.ConfidenceMedium
		}
		out = append(out, c)
	}
	return out
}

// fetchMethod reads the verb from a fetch options object.
//
// Returns known=false when an options object was opened but its `method:` was
// neither present nor ruled out within the lines examined. When the object is
// closed without stating one — or when there is no options object at all — the
// verb is GET, which is fetch's documented default and therefore a fact about
// the API rather than an assumption about this call, exactly as Flask's GET
// default is treated in extractPython.
const fetchOptionsWindow = 8

func fetchMethod(lines []string, at int, rest string) (string, bool) {
	if m := jsMethodOption.FindStringSubmatch(rest); m != nil {
		return verbOrEmpty(m[1]), true
	}
	depth := 1 + parenDelta(rest)
	if depth <= 0 {
		// The call closed on its own line without naming a verb, so there is
		// nothing left to read and GET is the answer rather than a shrug.
		return "GET", true
	}
	for i := at + 1; i < len(lines) && i <= at+fetchOptionsWindow; i++ {
		if m := jsMethodOption.FindStringSubmatch(lines[i]); m != nil {
			return verbOrEmpty(m[1]), true
		}
		if depth += parenDelta(lines[i]); depth <= 0 {
			return "GET", true
		}
	}
	return "", false
}

// parenDelta counts parentheses outside string literals and line comments,
// which is how the end of a call spanning several lines is found.
func parenDelta(s string) int {
	delta := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == '\\' && quote != '`' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			quote = c
		case '/':
			if i+1 < len(s) && s[i+1] == '/' {
				return delta
			}
		case '(':
			delta++
		case ')':
			delta--
		}
	}
	return delta
}

// inString reports whether a position sits inside a string literal.
//
// This is the gate that keeps a call out of a log line. `console.log('called
// fetch("/api/x")')` contains the exact spelling this package matches on, and
// reporting it would put an outbound dependency in an impact report that the
// code never makes — the documentation-comment failure the route extractor
// already guards against, arriving through a different door.
func inString(line string, pos int) bool {
	var quote byte
	for i := 0; i < len(line) && i < pos; i++ {
		c := line[i]
		if quote != 0 {
			if c == '\\' && quote != '`' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '"' || c == '\'' || c == '`' {
			quote = c
		}
	}
	return quote != 0
}

// verbOrEmpty keeps a verb this package does not report — HEAD, OPTIONS, or a
// typo — out of the Method field, rather than letting an arbitrary string
// become an HTTP method. The call is still real, so it is reported as ANY.
func verbOrEmpty(v string) string {
	u := strings.ToUpper(v)
	if httpMethods[u] {
		return u
	}
	return ""
}

// ---------------------------------------------------------------- Go

var (
	// http.NewRequest("POST", url, body) / http.NewRequestWithContext(ctx, ...)
	goNewRequest = regexp.MustCompile(`\bhttp\s*\.\s*NewRequest(WithContext)?\s*\(`)
	// http.Get(url), client.Post(url, ...), s.httpClient.Get(url).
	//
	// The receiver is captured and gated: a bare `.Get(` matches every cache,
	// map and repository in a Go codebase, so the receiver must name itself an
	// http thing or a client. That misses `c.Get(url)` on a client called `c`,
	// and missing it is the right trade — see the package comment.
	goClientVerb = regexp.MustCompile(`\b([\w.]+)\s*\.\s*(Get|Post|PostForm)\s*\(`)
	// http.MethodPost as the method argument.
	goMethodConst = regexp.MustCompile(`\bhttp\s*\.\s*Method(Get|Post|Put|Patch|Delete)\b`)
)

func extractGoClient(lines []string) []Call {
	var out []Call
	for i, line := range lines {
		if isComment(line) {
			continue
		}

		if loc := goNewRequest.FindStringSubmatchIndex(line); loc != nil && !inString(line, loc[0]) {
			args := splitArgs(line[loc[1]:])
			if loc[2] >= 0 {
				// WithContext: drop the ctx argument so the pair below is the
				// same in both spellings.
				if len(args) > 0 {
					args = args[1:]
				}
			}
			method := ""
			if len(args) > 0 {
				method = goMethodArg(args[0])
			}
			lit, dyn := "", true
			if len(args) > 1 {
				lit, dyn = literalArg(args[1])
			}
			out = append(out, newCall(method, lit, dyn, ClientGoNetHTTP, i+1))
			continue
		}

		m := goClientVerb.FindStringSubmatchIndex(line)
		if m == nil || inString(line, m[0]) {
			continue
		}
		if !looksLikeHTTPClient(line[m[2]:m[3]]) {
			continue
		}
		verb := "GET"
		if line[m[4]:m[5]] != "Get" {
			verb = "POST"
		}
		lit, dyn := literalArg(line[m[1]:])
		if !dyn && !looksLikeURL(lit) {
			// A literal first argument that is not a URL means this was never an
			// HTTP call — `client.Get("session-key")` is a cache lookup on a
			// receiver that happens to be named well. The receiver gate alone
			// would report it as a request to a path that does not exist.
			continue
		}
		out = append(out, newCall(verb, lit, dyn, ClientGoNetHTTP, i+1))
	}
	return out
}

// goMethodArg reads the verb from a net/http method argument, which is either a
// string literal or an http.MethodX constant. A variable yields "", reported as
// ANY, because the verb is chosen at run time and no value here is knowable.
func goMethodArg(arg string) string {
	if m := goMethodConst.FindStringSubmatch(arg); m != nil {
		return strings.ToUpper(m[1])
	}
	if lit, dyn := literalArg(arg); !dyn {
		return verbOrEmpty(lit)
	}
	return ""
}

// looksLikeHTTPClient is the receiver gate. The final identifier must say what
// it is: `http`, `client`, `httpClient`, `apiClient`.
func looksLikeHTTPClient(recv string) bool {
	r := strings.ToLower(lastIdent(recv))
	return strings.Contains(r, "http") || strings.Contains(r, "client")
}

func looksLikeURL(s string) bool {
	return strings.HasPrefix(s, "/") ||
		strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// ---------------------------------------------------------------- attribution

// attributeCalls names the declaration containing each call.
//
// The rule is line containment, the same one internal/analyze applies to SQL
// statements: the innermost declaration whose body covers the line. The
// difference is what supplies the span. That package has the file's symbol
// table and uses real ranges; this one has text, so the span is tracked by
// brace depth from the declaration that opened it.
//
// The approximation is worth naming. A body that opens and closes on one line
// pushes no frame, so a call on that line is attributed to the declaration on
// it — right for `const load = () => fetch("/a")`, which is how half of a React
// codebase is written. A body with no braces at all that continues onto the
// next line is NOT tracked, and a call there is attributed to whatever encloses
// the declaration, or to nothing. That is a weaker answer, never a wrong one:
// the outer declaration really does contain the line.
//
// A caller holding the symbol table should overwrite Caller from it. This is
// the fallback for a caller that does not.
func attributeCalls(lang string, lines []string, calls []Call) []Call {
	if len(calls) == 0 {
		return calls
	}
	owners := enclosingNames(lang, lines)
	for i := range calls {
		if l := calls[i].Line - 1; l >= 0 && l < len(owners) {
			calls[i].Caller = owners[l]
		}
	}
	return calls
}

type declFrame struct {
	name  string
	depth int
}

// enclosingNames returns, for each line, the innermost declaration containing it.
func enclosingNames(lang string, lines []string) []string {
	owners := make([]string, len(lines))
	var stack []declFrame
	depth := 0

	for i, line := range lines {
		name := declName(lang, line)

		owner := name
		if owner == "" && len(stack) > 0 {
			owner = stack[len(stack)-1].name
		}
		owners[i] = owner

		before := depth
		depth += braceDelta(line)
		if depth < 0 {
			depth = 0
		}
		if name != "" && depth > before {
			stack = append(stack, declFrame{name: name, depth: before})
		}
		for len(stack) > 0 && depth <= stack[len(stack)-1].depth {
			stack = stack[:len(stack)-1]
		}
	}
	return owners
}

var (
	goFuncDecl = regexp.MustCompile(`^\s*func\s+(?:\(\s*\w+\s+[\*\w.\[\]]+\s*\)\s*)?(\w+)\s*[\(\[]`)
	// function load() / export default async function Page()
	jsFunctionDecl = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*(\w+)\s*\(`)
	// const load = async () => / let f = function () / export const C: FC = () =>
	jsAssignedFunc = regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+(\w+)\s*(?::[^=]*)?=\s*(?:async\s+)?(?:function\b|\(|\w+\s*=>)`)
	// A class or object method: `async load(id: string) {`.
	jsMethodDecl = regexp.MustCompile(`^\s*(?:(?:public|private|protected|static|readonly|async)\s+)*(\w+)\s*\([^()]*\)\s*(?::[^{;]*)?\{`)
)

// jsNotADecl are the keywords jsMethodDecl would otherwise read as method
// names. `if (ok) {` has the exact shape of a method declaration, and naming a
// call's caller `if` is worse than leaving it unattributed.
var jsNotADecl = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "catch": true,
	"do": true, "else": true, "return": true, "function": true, "with": true,
}

func declName(lang, line string) string {
	if lang == "go" {
		if m := goFuncDecl.FindStringSubmatch(line); m != nil {
			return m[1]
		}
		return ""
	}
	if m := jsFunctionDecl.FindStringSubmatch(line); m != nil {
		return m[1]
	}
	if m := jsAssignedFunc.FindStringSubmatch(line); m != nil {
		return m[1]
	}
	if m := jsMethodDecl.FindStringSubmatch(line); m != nil && !jsNotADecl[m[1]] {
		return m[1]
	}
	return ""
}

// braceDelta counts braces that open and close a body, ignoring those inside
// string literals and line comments so a path like "/a/{id}" does not open one.
func braceDelta(line string) int {
	delta := 0
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		if quote != 0 {
			if c == '\\' && quote != '`' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			quote = c
		case '/':
			if i+1 < len(line) && line[i+1] == '/' {
				return delta
			}
		case '{':
			delta++
		case '}':
			delta--
		}
	}
	return delta
}

// ---------------------------------------------------------------- helpers

func newCall(method, path string, dynamic bool, client Client, line int) Call {
	c := Call{
		Method: method, Path: path, Client: client, Line: line,
		Dynamic: dynamic, Confidence: model.ConfidenceHigh,
	}
	if dynamic {
		// A partially-known target is still worth knowing about, and is reported
		// with a lower confidence for the same reason a partially-known route is.
		c.Confidence = model.ConfidenceMedium
	}
	return c
}

func isComment(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "//") || strings.HasPrefix(t, "*") ||
		strings.HasPrefix(t, "/*") || strings.HasPrefix(t, "#")
}

// literalArg reads one argument, given the text immediately after the opening
// paren or after a comma.
//
// It returns the literal value and whether the argument is assembled rather
// than literal. A quoted string followed by `+`, a template literal containing
// `${`, and anything that is not a string at all are all dynamic; the first two
// still carry the literal prefix the author wrote.
func literalArg(s string) (string, bool) {
	s = strings.TrimLeft(s, " \t")
	if s == "" {
		return "", true
	}
	switch s[0] {
	case '"', '\'':
		lit, rest, closed := scanQuoted(s)
		if !closed {
			// The literal does not end on this line, so what follows it — a
			// concatenation, another argument — is unknown.
			return lit, true
		}
		return lit, strings.HasPrefix(strings.TrimLeft(rest, " \t"), "+")
	case '`':
		body := s[1:]
		if j := strings.Index(body, "${"); j >= 0 {
			return body[:j], true
		}
		if i := strings.IndexByte(body, '`'); i >= 0 {
			return body[:i], strings.HasPrefix(strings.TrimLeft(body[i+1:], " \t"), "+")
		}
		return body, true
	}
	return "", true
}

// scanQuoted reads a quoted string, honouring backslash escapes so `"a\"b"`
// yields `a\"b` rather than stopping at the escaped quote and reporting a path
// the author never wrote.
func scanQuoted(s string) (lit, rest string, closed bool) {
	q := s[0]
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			b.WriteByte(c)
			i++
			b.WriteByte(s[i])
			continue
		}
		if c == q {
			return b.String(), s[i+1:], true
		}
		b.WriteByte(c)
	}
	return b.String(), "", false
}

// splitArgs splits a call's arguments at top-level commas, stopping at the
// closing paren. Nested calls, literals and brace-delimited values are skipped
// over so `http.NewRequest(http.MethodPost, u, bytes.NewReader(b))` splits into
// three arguments rather than four.
func splitArgs(s string) []string {
	var args []string
	depth, start := 0, 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == '\\' && quote != '`' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			quote = c
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if c == ')' && depth == 0 {
				return append(args, s[start:i])
			}
			depth--
		case ',':
			if depth == 0 {
				args = append(args, s[start:i])
				start = i + 1
			}
		}
	}
	// The call does not close on this line; what was read is still usable.
	return append(args, s[start:])
}
