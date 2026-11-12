package pipeline

import "github.com/satyendrakumarsingh/repocortex/pkg/model"

// resolveFQNs fills in the default qualified name for symbols that have none,
// so that the identity used for de-duplication is the identity the registry
// will store.
func resolveFQNs(path string, syms []model.Symbol) {
	for i := range syms {
		if syms[i].FQN == "" {
			// Until the resolver assigns a real qualified name, the path plus
			// the symbol name is a stable, collision-resistant stand-in. It is
			// stable across runs, which the registry digest depends on.
			syms[i].FQN = model.DefaultFQN(path, syms[i].Name)
		}
	}
}

// collapseDuplicateDeclarations drops symbols that would collide on the
// registry's notion of symbol identity: (index run, fqn, file, start line).
//
// The schema treats those four as one symbol. A tags query can legitimately
// produce two declarations that agree on all of them, because they differ only
// in COLUMN, and the constraint does not include it. Real example, from a
// TypeScript end-to-end test:
//
//	async function json<T>(res: { ...; json(): Promise<any>; ... }, what: string)
//
// Line 152 declares a function called `json` and, inside that function's own
// parameter type, a method signature also called `json`. Both are real
// declarations; both land on (file, "path:json", 152).
//
// Before this, the second insert failed the UNIQUE constraint and took the
// WHOLE index down — `repocortex index` exited 1 at 80% on an 8,094-file
// repository and committed nothing, so one line of one test file made the tool
// unusable on that project.
//
// The survivor is the first, which is the outermost declaration in tags order
// and the one a user searching for the name means. Nothing answerable is lost:
// every query RepoCortex serves locates a symbol by file and line, so two symbols
// sharing both are indistinguishable in every result it can produce. The count
// is returned rather than swallowed, so the gap is reportable instead of
// invisible.
//
// Widening the constraint to include start_col would be the fuller fix. It
// needs a table rebuild — SQLite cannot alter a UNIQUE constraint in place, and
// the implicit index cannot be dropped — and dropping `symbols` fires
// ON DELETE CASCADE across edges, symbol_references, table_accesses and
// config_usages. That is a migration to write deliberately, not one to write
// while chasing an index failure.
func collapseDuplicateDeclarations(syms []model.Symbol) ([]model.Symbol, int) {
	if len(syms) < 2 {
		return syms, 0
	}
	type key struct {
		fqn  string
		line int
	}
	seen := make(map[key]struct{}, len(syms))
	out := syms[:0:0]
	dropped := 0
	for _, s := range syms {
		k := key{fqn: s.FQN, line: s.Range.Start.Line}
		if _, dup := seen[k]; dup {
			dropped++
			continue
		}
		seen[k] = struct{}{}
		out = append(out, s)
	}
	return out, dropped
}
