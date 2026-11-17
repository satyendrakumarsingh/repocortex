package cli

import (
	"bufio"
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The receivers fixture (fixtures/receivers) is the regression suite for one
// defect: a call that has a receiver or a qualifier being bound as if it had
// none. Every marked line is a wrong binding found on a public project
// (docs/verification/2026-11-12), and the marker says what the index must
// hold for it instead. fixtures/receivers/README.md states the grammar.
//
// The suite is deliberately end to end — parse, store, resolve, read the
// tables — because the defect was never visible to a unit test. Tests that
// built a reference by hand set its receiver directly, and passed; only the
// path through the parser, which left the receiver empty for every chained or
// qualified call, produced the wrong edges (A18, in internal/resolve/infer).

type receiverMarker struct {
	file string
	line int

	// verb is "call", "nocall", "super" or "nosuper". A super marker says the
	// supertype clause on that line names <name> and the index holds a
	// hierarchy edge (EXTENDS or IMPLEMENTS) to the def labelled target, or
	// none. A nosuper marker says only that no hierarchy edge to a type called
	// <name> starts on that line, whether or not the clause was extracted.
	verb, name string

	// exported is the name a renamed import's use is exported by, or "".
	exported string

	// target is a def label, or "none" or "external". instantiates is true
	// for `-> new <ID>`. tier is "" when the marker does not require one.
	target       string
	instantiates bool
	tier         string
}

type receiverDef struct {
	file string
	line int
}

var (
	// A marker comment is the last comment on its line, in any of the three
	// comment syntaxes the fixture's languages use.
	receiverComment = regexp.MustCompile(`(?:^|\s)(?://|#|/\*)\s*((?:def|call|nocall|super|nosuper)\s.*?)\s*(?:\*/)?\s*$`)
	receiverCall    = regexp.MustCompile(`^call\s+(\S+)\s*->\s*(new\s+)?(\S+)(?:\s+(high|medium|low))?$`)
	receiverSuper   = regexp.MustCompile(`^super\s+(\S+)\s*->\s*(\S+)$`)
)

func readReceiverMarkers(t *testing.T, root string) (map[string]receiverDef, []receiverMarker) {
	t.Helper()
	defs := map[string]receiverDef{}
	var markers []receiverMarker
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == IndexDir {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".md") || strings.HasSuffix(p, ".json") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		f, err := os.Open(p) //nolint:gosec // a fixture copy under t.TempDir
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		sc := bufio.NewScanner(f)
		for line := 1; sc.Scan(); line++ {
			m := receiverComment.FindStringSubmatch(sc.Text())
			if m == nil {
				continue
			}
			for _, part := range strings.Split(m[1], ";") {
				part = strings.TrimSpace(part)
				switch {
				case strings.HasPrefix(part, "def "):
					id := strings.TrimSpace(strings.TrimPrefix(part, "def "))
					if prev, dup := defs[id]; dup {
						t.Fatalf("def marker %q at %s:%d repeats %s:%d", id, rel, line, prev.file, prev.line)
					}
					defs[id] = receiverDef{file: rel, line: line}
				case strings.HasPrefix(part, "nocall "):
					markers = append(markers, receiverMarker{file: rel, line: line, verb: "nocall",
						name: strings.TrimSpace(strings.TrimPrefix(part, "nocall "))})
				case strings.HasPrefix(part, "nosuper "):
					markers = append(markers, receiverMarker{file: rel, line: line, verb: "nosuper",
						name: strings.TrimSpace(strings.TrimPrefix(part, "nosuper ")), target: "none"})
				case strings.HasPrefix(part, "super "):
					c := receiverSuper.FindStringSubmatch(part)
					if c == nil {
						t.Fatalf("%s:%d: malformed marker %q", rel, line, part)
					}
					markers = append(markers, receiverMarker{file: rel, line: line, verb: "super",
						name: c[1], target: c[2]})
				case strings.HasPrefix(part, "call "):
					c := receiverCall.FindStringSubmatch(part)
					if c == nil {
						t.Fatalf("%s:%d: malformed marker %q", rel, line, part)
					}
					// `call Local=Exported`: a use of an import renamed
					// `Exported as Local`. The reference is named Local; an
					// edge to a definition named Exported or Local is its.
					name, exported, _ := strings.Cut(c[1], "=")
					markers = append(markers, receiverMarker{file: rel, line: line, verb: "call",
						name: name, exported: exported, instantiates: c[2] != "", target: c[3], tier: c[4]})
				default:
					t.Fatalf("%s:%d: unknown marker %q", rel, line, part)
				}
			}
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range markers {
		if (m.verb != "call" && m.verb != "super") || m.target == "none" || m.target == "external" {
			continue
		}
		if _, ok := defs[m.target]; !ok {
			t.Fatalf("%s:%d names target %q, which no def marker declares", m.file, m.line, m.target)
		}
	}
	return defs, markers
}

type receiverEdge struct {
	kind, confidence, file, detail string
	line                           int
}

func receiverEdgesAt(t *testing.T, db *sql.DB, m receiverMarker) []receiverEdge {
	t.Helper()
	rows, err := db.Query(`
		SELECT e.kind, e.confidence, f.path, s.start_line, COALESCE(e.detail, '')
		  FROM edges e
		  JOIN symbols s ON s.id = e.dst_id
		  JOIN files f ON f.id = s.file_id
		 WHERE e.source = 'source_code'
		   -- Calls only. A call reference always becomes CALLS or INSTANTIATES;
		   -- a REFERENCES edge on the same line is a type use ("Visitor j =
		   -- new Visitor() {...}" names Visitor as j's type), which is right
		   -- and not what this suite audits.
		   AND e.kind IN ('CALLS', 'INSTANTIATES')
		   AND e.evidence_file = ? AND e.evidence_line = ?
		   AND (s.name = ? OR s.name = ? OR (s.name = 'apply' AND s.fqn LIKE '%:' || ? || '.apply'))
		 ORDER BY e.id`, m.file, m.line, m.name, m.exported, m.name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []receiverEdge
	for rows.Next() {
		var e receiverEdge
		if err := rows.Scan(&e.kind, &e.confidence, &e.file, &e.line, &e.detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestReceiverFixtureBindsNothingWrongly is the gate.
func TestReceiverFixtureBindsNothingWrongly(t *testing.T) {
	// fixtureCopy initialises, indexes and resolves the copy.
	// The heuristic resolver is what this suite audits, so the compiler passes
	// stay off: the Go type check would raise the edges it reads by source.
	root := fixtureCopyHeuristic(t, "receivers")
	defs, markers := readReceiverMarkers(t, root)
	db := corpusDB(t, root)

	if len(markers) < 50 {
		t.Fatalf("only %d markers read; the fixture or the marker grammar is broken", len(markers))
	}

	for _, m := range markers {
		if m.verb == "super" || m.verb == "nosuper" {
			checkHierarchyMarker(t, db, defs, m)
			continue
		}
		var refs, external int
		if err := db.QueryRow(`
			SELECT COUNT(*), COALESCE(SUM(sr.external), 0)
			  FROM symbol_references sr JOIN files f ON f.id = sr.file_id
			 WHERE f.path = ? AND sr.start_line = ? AND sr.name = ? AND sr.kind LIKE '%call%'`,
			m.file, m.line, m.name).Scan(&refs, &external); err != nil {
			t.Fatal(err)
		}
		edges := receiverEdgesAt(t, db, m)

		if m.verb == "nocall" {
			if refs != 0 {
				t.Errorf("%s:%d: %q is recorded as %d call reference(s); it is the receiver of a call, not a call",
					m.file, m.line, m.name, refs)
			}
			continue
		}
		if refs == 0 {
			t.Errorf("%s:%d: no call reference named %q was recorded at all", m.file, m.line, m.name)
			continue
		}

		switch m.target {
		case "none", "external":
			for _, e := range edges {
				t.Errorf("%s:%d: %s must bind to nothing, and is bound %s at %s to %s:%d (%s)",
					m.file, m.line, m.name, e.kind, e.confidence, e.file, e.line, e.detail)
			}
			if m.target == "external" && external == 0 {
				t.Errorf("%s:%d: %s leaves the repository and is not recorded as external",
					m.file, m.line, m.name)
			}
			if m.target == "none" && external != 0 {
				t.Errorf("%s:%d: %s is recorded as external; nothing shows it leaves the repository",
					m.file, m.line, m.name)
			}
		default:
			want := defs[m.target]
			wantKind := "CALLS"
			if m.instantiates {
				wantKind = "INSTANTIATES"
			}
			found := false
			for _, e := range edges {
				if e.file != want.file || e.line != want.line {
					t.Errorf("%s:%d: %s is bound %s at %s to %s:%d (%s); it reaches %s at %s:%d",
						m.file, m.line, m.name, e.kind, e.confidence, e.file, e.line, e.detail,
						m.target, want.file, want.line)
					continue
				}
				found = true
				if e.kind != wantKind {
					t.Errorf("%s:%d: %s -> %s is a %s edge, want %s", m.file, m.line, m.name, m.target, e.kind, wantKind)
				}
				if m.tier != "" && e.confidence != m.tier {
					t.Errorf("%s:%d: %s -> %s is bound at %s (%s), want %s",
						m.file, m.line, m.name, m.target, e.confidence, e.detail, m.tier)
				}
			}
			if !found {
				t.Errorf("%s:%d: %s produced no edge to %s (%s:%d)", m.file, m.line, m.name, m.target, want.file, want.line)
			}
		}
	}

	// Three properties of the whole graph, beyond any one marker.
	//
	// A CALLS edge points at something callable.
	if n := scalar(t, db, `
		SELECT COUNT(*) FROM edges e JOIN symbols s ON s.id = e.dst_id
		 WHERE e.kind = 'CALLS' AND e.source = 'source_code' AND s.kind NOT IN ('function', 'method')
		   AND NOT (s.kind = 'variable' AND COALESCE(e.detail, '') LIKE '%the object is called%')`); n != 0 {
		t.Errorf("%d CALLS edge(s) point at something that is not a function or a method", n)
	}
	// An INSTANTIATES edge points at a type.
	if n := scalar(t, db, `
		SELECT COUNT(*) FROM edges e JOIN symbols s ON s.id = e.dst_id
		 WHERE e.kind = 'INSTANTIATES' AND s.kind <> 'class'`); n != 0 {
		t.Errorf("%d INSTANTIATES edge(s) point at something that is not a type", n)
	}
	// No call crosses into a language its own cannot call. C and C++ link
	// together; nothing else in this fixture does.
	rows, err := db.Query(`
		SELECT sf.path, sf.language, df.path, df.language
		  FROM edges e
		  JOIN symbols ss ON ss.id = e.src_id JOIN files sf ON sf.id = ss.file_id
		  JOIN symbols ds ON ds.id = e.dst_id JOIN files df ON df.id = ds.file_id
		 WHERE e.source = 'source_code' AND e.kind IN ('CALLS', 'INSTANTIATES')
		   AND sf.language <> df.language`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var srcPath, srcLang, dstPath, dstLang string
		if err := rows.Scan(&srcPath, &srcLang, &dstPath, &dstLang); err != nil {
			t.Fatal(err)
		}
		if (srcLang == "c" || srcLang == "cpp") && (dstLang == "c" || dstLang == "cpp") {
			continue
		}
		t.Errorf("a call in %s (%s) is bound to %s (%s)", srcPath, srcLang, dstPath, dstLang)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// checkHierarchyMarker verifies one `super` marker: the supertype clause on the
// line was extracted as a reference, and it produced an EXTENDS or IMPLEMENTS
// edge to the labelled definition and to nothing else, or to nothing at all
// when the target is "none".
func checkHierarchyMarker(t *testing.T, db *sql.DB, defs map[string]receiverDef, m receiverMarker) {
	t.Helper()
	var refs int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM symbol_references sr JOIN files f ON f.id = sr.file_id
		 WHERE f.path = ? AND sr.start_line = ? AND sr.name = ?
		   AND (sr.kind LIKE '%extend%' OR sr.kind LIKE '%inherit%' OR sr.kind LIKE '%implementation%')`,
		m.file, m.line, m.name).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if refs == 0 && m.verb == "super" {
		t.Errorf("%s:%d: no supertype reference named %q was extracted", m.file, m.line, m.name)
		return
	}
	rows, err := db.Query(`
		SELECT e.kind, f.path, s.start_line
		  FROM edges e
		  JOIN symbols s ON s.id = e.dst_id
		  JOIN files f ON f.id = s.file_id
		 WHERE e.source = 'source_code' AND e.kind IN ('EXTENDS', 'IMPLEMENTS')
		   AND e.evidence_file = ? AND e.evidence_line = ? AND s.name = ?`,
		m.file, m.line, m.name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	found := false
	for rows.Next() {
		var kind, path string
		var line int
		if err := rows.Scan(&kind, &path, &line); err != nil {
			t.Fatal(err)
		}
		if m.target == "none" {
			t.Errorf("%s:%d: %s must have no hierarchy edge, and has %s to %s:%d", m.file, m.line, m.name, kind, path, line)
			continue
		}
		want := defs[m.target]
		if path != want.file || line != want.line {
			t.Errorf("%s:%d: %s has a %s edge to %s:%d; it is %s at %s:%d",
				m.file, m.line, m.name, kind, path, line, m.target, want.file, want.line)
			continue
		}
		found = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if m.target != "none" && !found {
		t.Errorf("%s:%d: %s produced no hierarchy edge to %s", m.file, m.line, m.name, m.target)
	}
}
