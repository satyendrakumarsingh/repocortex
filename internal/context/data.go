package context

import (
	"regexp"
	"sort"
	"strings"
)

// tableRef is one table a unit reads or writes, with the evidence for it.
type tableRef struct {
	Name string

	// Write distinguishes "this code changes the table" from "this code reads
	// it". For a task about idempotency that distinction IS the answer: the
	// duplicate-suppression has to happen where rows are written.
	Write bool

	// Line is where the statement appears in the referring file.
	Line int
}

// sqlStatement finds the table name in a DML or DDL statement.
//
// Anchored on the keyword rather than on the whole statement grammar: parsing
// SQL properly is a language backend's job (spec tiers it as Tier 3 framework
// awareness), and this only needs the operand of FROM / JOIN / INTO / UPDATE /
// CREATE TABLE to know which tables a call path touches.
var sqlStatement = regexp.MustCompile(
	`(?is)\b(from|join|into|update|delete\s+from|create\s+table(?:\s+if\s+not\s+exists)?)\s+` +
		`` + "`" + `?"?([a-zA-Z_][a-zA-Z0-9_]*)` + "`" + `?"?`)

// stringLiteral matches a double-quoted literal with no embedded newline.
//
// SQL in application code lives in string literals, and restricting the scan to
// them is what keeps the extractor from firing on host-language syntax. Go's
// `for ... range` and Python's `from x import y` both contain a keyword this
// regexp looks for; neither is inside a string.
var stringLiteral = regexp.MustCompile(`"([^"\n\\]|\\.){4,}"`)

// sqlNoise are words that follow a matched keyword without being table names.
var sqlNoise = map[string]bool{
	"select": true, "where": true, "set": true, "values": true, "table": true,
	"index": true, "on": true, "as": true, "and": true, "or": true, "not": true,
	"null": true, "by": true, "order": true, "group": true, "limit": true,
	"exists": true, "if": true, "join": true, "inner": true, "outer": true,
	"left": true, "right": true, "full": true, "cross": true, "dual": true,
}

// extractTables finds the tables referenced by one file's content.
//
// language decides where to look: a .sql file is SQL throughout, anything else
// is scanned only inside string literals.
func extractTables(language, content string) []tableRef {
	regions := [][]int{{0, len(content)}}
	if !strings.EqualFold(language, "sql") {
		regions = stringLiteral.FindAllStringIndex(content, -1)
	}
	if len(regions) == 0 {
		return nil
	}

	// Regions are index pairs rather than substrings so each match's line
	// number is the real one, even when the same literal appears twice.
	seen := map[string]*tableRef{}
	for _, r := range regions {
		region := content[r[0]:r[1]]
		for _, m := range sqlStatement.FindAllStringSubmatchIndex(region, -1) {
			keyword := strings.ToLower(strings.Join(strings.Fields(region[m[2]:m[3]]), " "))
			name := region[m[4]:m[5]]
			if sqlNoise[strings.ToLower(name)] {
				continue
			}
			line := 1 + strings.Count(content[:r[0]+m[0]], "\n")
			write := keyword == "into" || keyword == "update" ||
				strings.HasPrefix(keyword, "delete") || strings.HasPrefix(keyword, "create table")

			key := strings.ToLower(name)
			if ex, ok := seen[key]; ok {
				// A table both read and written is reported as written: the
				// stronger relationship is the one that matters for a change.
				ex.Write = ex.Write || write
				continue
			}
			seen[key] = &tableRef{Name: name, Write: write, Line: line}
		}
	}

	out := make([]tableRef, 0, len(seen))
	for _, k := range sortedKeys(seen) {
		out = append(out, *seen[k])
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
