package multirepo

import "strings"

// Route canonicalisation for the cross-repository hop.
//
// WHY THIS IS A SECOND COPY OF RULES internal/registry ALREADY HAS. The
// canonicalisation that binds a call to an endpoint inside one repository lives
// in unexported functions of internal/registry (canonSegments, isParamSegment,
// canonMethod, segmentsMatch). This package cannot import them and does not own
// that package. The two must nevertheless agree segment-for-segment, because a
// call that binds locally and a call that binds across a boundary have to be
// canonicalised the same way or the same route text would mean two different
// things depending on which side of a repository line it fell. crossRepoRoutes
// asserts that agreement on the spellings that matter; if internal/registry
// ever exports its matcher, this file should be deleted in favour of it.
//
// The rules, restated so a reader of THIS file can check them: scheme and host
// go (a call to https://api.example.com/v1/orders and a route /v1/orders are
// the same route when the host is that service, and the host is not knowable
// from the text either way); query and fragment go, being arguments rather than
// identity; case goes; a trailing slash goes; a doubled slash is a typo rather
// than an empty segment; and every parameter spelling collapses to one
// wildcard, so {id}, :id, <int:id>, [id] and * are one concept rather than five.

// canonSegments reduces a path to the segments matching compares.
func canonSegments(p string) []string {
	p = strings.TrimSpace(p)
	if i := strings.Index(p, "://"); i >= 0 {
		rest := p[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			p = rest[j:]
		} else {
			p = "/"
		}
	}
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	if i := strings.IndexByte(p, '#'); i >= 0 {
		p = p[:i]
	}
	p = strings.ToLower(strings.Trim(p, "/"))
	if p == "" {
		return nil
	}
	raw := strings.Split(p, "/")
	segs := make([]string, 0, len(raw))
	for _, s := range raw {
		if s == "" {
			continue
		}
		if isParamSegment(s) {
			s = "*"
		}
		segs = append(segs, s)
	}
	return segs
}

// isParamSegment recognises every parameter spelling in use across the
// frameworks route extraction covers, plus the client-side ones that reach here
// through a path written in a call.
//
//	{id}      Spring, Express 5, FastAPI, .NET
//	{id:\d+}  Spring with a regex constraint
//	:id       Express 4, chi, gin
//	<int:id>  Flask
//	[id]      Next.js file-system routing
//	*, **     catch-alls
func isParamSegment(s string) bool {
	switch {
	case s == "*" || s == "**":
		return true
	case strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}"):
		return true
	case strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">"):
		return true
	case strings.HasPrefix(s, ":") && len(s) > 1:
		return true
	case strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]"):
		return true
	default:
		return false
	}
}

// canonMethod folds the two spellings of "no verb stated" into one.
func canonMethod(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	if m == "any" {
		return ""
	}
	return m
}

// methodsMatch reports whether two verbs can describe the same request. An
// empty verb is ANY on either side: the extractor could not read it, which is
// not evidence that it differs — but it IS weaker evidence that it agrees, and
// matchLegConfidence charges a tier for it.
func methodsMatch(a, b string) bool { return a == "" || b == "" || a == b }

// segmentsMatch compares a route's segments against a call's and reports how
// many wildcards were spent.
//
// A wildcard absorbs exactly ONE segment. A catch-all that swallowed the
// remainder would make /api/* match every call in the estate, which is how a
// route table with one wildcard produces an impact report in which everything
// depends on everything.
func segmentsMatch(route, call []string) (wildcards int, ok bool) {
	if len(route) != len(call) {
		return 0, false
	}
	for i, rs := range route {
		if rs == "*" {
			wildcards++
			continue
		}
		if rs != call[i] {
			return 0, false
		}
	}
	return wildcards, true
}

// prefixMatches reports whether a route begins with the literal segments a
// dynamic call revealed.
func prefixMatches(route, prefix []string) bool {
	if len(route) < len(prefix) {
		return false
	}
	for i, s := range prefix {
		if route[i] != "*" && route[i] != s {
			return false
		}
	}
	return true
}

// splitSignature separates an endpoint node's name — "GET /api/orders/{id}",
// or "ANY /health" where the framework stated no verb — into verb and path.
//
// A signature with no space is treated as a bare path: an endpoint written by
// an extractor that does not spell the verb must still be matchable, and
// reading its whole name as a verb would make it match nothing.
func splitSignature(sig string) (method, path string) {
	sig = strings.TrimSpace(sig)
	if i := strings.IndexByte(sig, ' '); i >= 0 {
		return sig[:i], strings.TrimSpace(sig[i+1:])
	}
	if strings.HasPrefix(sig, "/") {
		return "", sig
	}
	return sig, ""
}
