package db

import "strings"

// ORM extraction: the same question as the SQL recogniser, asked of code that
// never spells a query out.
//
// # Why this exists
//
// The SQL recogniser answers "which code writes `transactions`" only for code
// that writes the word SELECT. Most application code does not: it calls
// `repo.save(t)`, `db.Model(&Transaction{}).Updates(...)`, or
// `session.query(Transaction)`. On a repository built on an ORM, `cortex db`
// reported almost nothing, and the silence was indistinguishable from a service
// that touches no data at all. That is the failure mode this package's own doc
// comment calls the worst one — a table that appears in no query looks exactly
// like a table nothing uses.
//
// # The same contract as SQL
//
// An ORM operation produces a Statement, not a new type. It carries the same
// five access modes, the same lowercased unqualified table names, and the same
// Certain flag, so the caller stages it through exactly the path raw SQL
// already uses and nothing downstream — resolver, registry, `cortex db`,
// impact — learns that ORMs exist.
//
// # Three ecosystems, chosen
//
// GORM, JPA/Hibernate and SQLAlchemy. Prisma, Sequelize and ActiveRecord are
// deliberately absent: each needs its own entity-declaration story (a schema
// DSL, a migration DSL, a convention with no declaration at all), and three
// done properly is worth more than six done by keyword.
//
// # Where the table name comes from
//
// In priority order: what the source states, then the framework's documented
// default, then nothing. The middle rung is only taken where the convention can
// be stated exactly, and it sets Certain=false, because a convention is
// configurable and a declaration is not.
//
//   - GORM: NamingStrategy snake_cases the struct name and pluralises it —
//     `User` -> `users`, `OrderItem` -> `order_items`. Configurable via
//     `SingularTable`, hence never Certain.
//   - JPA: with no `@Table(name=...)`, Spring Boot's
//     CamelCaseToUnderscoresNamingStrategy (the default physical naming
//     strategy in every Spring Boot application) lowercases the entity's simple
//     name and underscores its camel-case boundaries — `OrderItem` ->
//     `order_item`. Plain Hibernate's PhysicalNamingStrategyStandardImpl uses
//     the entity name verbatim instead, so the two disagree for multi-word
//     names and agree for single-word ones. Certain only when they agree.
//   - SQLAlchemy declarative has NO default: `__tablename__` is required and
//     omitting it raises InvalidRequestError, so there is nothing to fall back
//     to. Flask-SQLAlchemy's `db.Model` does supply one — camel-case to
//     snake_case, no pluralisation — and that fallback is taken only when the
//     class actually names `db.Model` as a base.
//
// An access whose table resolves to a name this repository never declares is
// still recorded. Migration 0003 exists for exactly that: "this service writes
// a table we do not own" is a fact, and dropping it makes the question
// unanswerable. What is NOT recorded is an access with no name at all —
// inventing one from a variable spelling is the fabrication the SQL side spends
// its whole false-positive gate avoiding.

// ORMStatement is one recognised ORM operation.
//
// It embeds Statement so a caller stages it identically to raw SQL; Line is
// carried alongside because the SQL side gets its line from the statement
// splitter and there is no splitter here.
type ORMStatement struct {
	Statement
	Line int
	// Framework is the ecosystem that was recognised: "gorm", "jpa" or
	// "sqlalchemy". Diagnostic only — it answers "why does cortex think this
	// line writes `orders`" without re-reading the file.
	Framework string
}

// ORMStatements extracts table accesses from ORM usage in one source file.
//
// The language is chosen by extension, and each extractor then gates on the
// framework's own spelling before it reports anything: a `.go` file that never
// mentions GORM produces nothing, and so does a `.java` file with no JPA
// annotation and no JPA import. That gate is the whole defence against the
// failure the SQL side documents — `Delete`, `save` and `query` are ordinary
// words, and a scan that trusted them would invent a table per method call.
func ORMStatements(path string, src []byte) []ORMStatement {
	if len(src) == 0 || len(src) > maxORMBytes {
		return nil
	}
	lower := strings.ToLower(path)
	var out []ORMStatement
	switch {
	case strings.HasSuffix(lower, ".go"):
		out = gormStatements(string(src))
	case strings.HasSuffix(lower, ".java"):
		out = jpaStatements(string(src))
	case strings.HasSuffix(lower, ".py"):
		out = sqlAlchemyStatements(string(src))
	case strings.HasSuffix(lower, ".prisma"):
		out = prismaSchemaStatements(string(src))
	case strings.HasSuffix(lower, ".ts"), strings.HasSuffix(lower, ".tsx"),
		strings.HasSuffix(lower, ".js"), strings.HasSuffix(lower, ".mjs"),
		strings.HasSuffix(lower, ".cjs"):
		out = prismaClientStatements(string(src))
	default:
		return nil
	}
	return normaliseORM(out)
}

// mapping is a resolved entity-to-table decision, and how sure it is.
type mapping struct {
	table string
	// certain is true only when the source STATES the table name. A name
	// derived from a naming convention is reported with certain=false, which
	// reaches the store as `certain = 0` and the reader as reduced confidence.
	certain bool
}

// emit builds one statement, and is the only place ORM results are constructed
// so the invariants the caller depends on hold in one spot: a lowercased
// unqualified table, a non-empty table list, and an access mode.
func emit(framework, verb string, access Access, line int, certain bool, tables ...string) (ORMStatement, bool) {
	st := ORMStatement{Line: line, Framework: framework}
	st.Verb = verb
	st.Access = access
	st.Certain = certain
	st.Tables = dedupeLower(tables)
	if len(st.Tables) == 0 || access == "" {
		return ORMStatement{}, false
	}
	return st, true
}

// normaliseORM drops repeats. One chain can name a table through both `Model`
// and its terminal argument, and an entity declared twice in one file (a
// mapped superclass and its subclass) reports the same table twice; counting
// either twice inflates a usage report without adding a fact.
func normaliseORM(in []ORMStatement) []ORMStatement {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	out := in[:0]
	for _, s := range in {
		key := s.Verb + "|" + string(s.Access) + "|" + strings.Join(s.Tables, ",") + "|" + itoaORM(s.Line)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, s)
	}
	return out
}

func itoaORM(n int) string {
	if n <= 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// snakeCase converts a camel- or Pascal-case identifier to snake_case.
//
// Shared by all three conventions because all three define it the same way: a
// boundary is a lower-to-upper transition, or the last capital of a run that is
// followed by a lowercase letter, so `HTTPServer` becomes `http_server` rather
// than `h_t_t_p_server`.
func snakeCase(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	r := []byte(s)
	for i := 0; i < len(r); i++ {
		c := r[i]
		upper := c >= 'A' && c <= 'Z'
		if upper && i > 0 {
			prev := r[i-1]
			prevLowerOrDigit := prev >= 'a' && prev <= 'z' || prev >= '0' && prev <= '9'
			nextLower := i+1 < len(r) && r[i+1] >= 'a' && r[i+1] <= 'z'
			if prevLowerOrDigit || (prev >= 'A' && prev <= 'Z' && nextLower) {
				b.WriteByte('_')
			}
		}
		if upper {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}

// isCompound reports whether an identifier has an internal camel-case boundary.
//
// This is the JPA question: the JPA-compliant and Spring Boot naming strategies
// produce the same lowercased name for `Customer` and different names for
// `OrderItem`, so a single-word entity can be reported as certain and a
// multi-word one cannot.
func isCompound(name string) bool {
	return strings.Contains(snakeCase(name), "_")
}

// pluralise applies the regular English rules jinzhu/inflection implements,
// which is the library GORM's default NamingStrategy uses.
//
// Only the regular rules plus the handful of irregulars that actually name
// tables. An entity whose plural this gets wrong produces a table name that
// binds to nothing and is reported unbound, which is the designed degradation
// rather than a silent loss.
func pluralise(s string) string {
	if s == "" {
		return ""
	}
	if p, ok := irregularPlurals[s]; ok {
		return p
	}
	switch {
	case strings.HasSuffix(s, "s"), strings.HasSuffix(s, "x"), strings.HasSuffix(s, "z"),
		strings.HasSuffix(s, "ch"), strings.HasSuffix(s, "sh"):
		return s + "es"
	case len(s) > 1 && strings.HasSuffix(s, "y") && !isVowel(s[len(s)-2]):
		return s[:len(s)-1] + "ies"
	}
	return s + "s"
}

func isVowel(c byte) bool {
	switch c {
	case 'a', 'e', 'i', 'o', 'u':
		return true
	}
	return false
}

var irregularPlurals = map[string]string{
	"person": "people", "child": "children", "man": "men", "woman": "women",
	"foot": "feet", "tooth": "teeth", "mouse": "mice", "goose": "geese",
	"datum": "data", "index": "indices", "matrix": "matrices",
	"vertex": "vertices", "status": "statuses",
}

// isTypeName reports whether an identifier could name an entity type: it must
// start with an uppercase letter, which is the one thing Go, Java and Python
// all spell the same way for a class.
func isTypeName(s string) bool {
	return s != "" && s[0] >= 'A' && s[0] <= 'Z'
}
