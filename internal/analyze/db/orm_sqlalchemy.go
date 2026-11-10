package db

import "strings"

// SQLAlchemy (and Flask-SQLAlchemy).
//
// # Why the fallback is narrower here than for GORM and JPA
//
// SQLAlchemy's declarative mapping has NO default table name. `__tablename__`
// is required, and a declarative class without it raises InvalidRequestError at
// import time. There is therefore nothing to fall back to, and an entity this
// file does not declare — a model imported from `models.py` and queried here —
// names a table this extractor cannot know. Those accesses are dropped rather
// than guessed: snake-casing the class name would be inventing a convention
// SQLAlchemy does not have.
//
// The one exception is Flask-SQLAlchemy, which DOES define a default for
// `db.Model` subclasses: the class name camel-case-to-snake-case, without
// pluralisation, so `OrderItem` -> `order_item`. That fallback is taken only
// when the class names `db.Model` as a base in this file, which is the
// framework stating the convention applies.
//
// # What is read
//
//   - `class Order(Base): __tablename__ = "orders"` and `__table__ = Table(...)`
//     declare the table, and the mapped attributes declare its columns.
//   - `orders = Table("orders", metadata, Column("id", Integer))` — the Core
//     spelling of the same declaration.
//   - `session.query(Order)`, `select(Order)`, `session.get(Order, id)` read.
//   - `session.add(order)`, `session.delete(order)`, `insert(Order)`,
//     `.query(Order).delete()` write.

// pyRead and pyWrite are the session operations, split by what they do. `query`
// reads; a `delete()` or `update()` terminating that same chain turns it into a
// write, which is why the chain is classified as a whole rather than per call.
var pyRead = map[string]bool{
	"query": true, "get": true, "scalars": true, "scalar": true,
}

var pyWrite = map[string]bool{
	"add": true, "add_all": true, "merge": true, "delete": true, "update": true,
	"insert": true, "bulk_save_objects": true, "bulk_insert_mappings": true,
	"bulk_update_mappings": true,
}

// pyConstructors are the SQLAlchemy Core statement constructors, which name
// their target table as their first argument.
var pyConstructors = map[string]Access{
	"select": AccessRead,
	"insert": AccessWrite,
	"update": AccessWrite,
	"delete": AccessWrite,
}

// pySessionFactories are the calls that produce a Session, used to recognise a
// session handle by what made it rather than by what it is called.
var pySessionFactories = map[string]bool{
	"Session": true, "sessionmaker": true, "scoped_session": true,
	"create_session": true, "async_sessionmaker": true, "AsyncSession": true,
}

func sqlAlchemyStatements(src string) []ORMStatement {
	toks := lexIndented(src)
	if !usesSQLAlchemy(toks) {
		return nil
	}
	match := matchIndex(toks)

	ents := pyEntities(toks, match)
	vars := pyVarTypes(toks)
	handles := pySessionHandles(toks)

	var out []ORMStatement

	// Declarations. Every table name here was stated in the source — either as
	// `__tablename__` or as Table's first argument — so these are certain, and
	// a conventional Flask-SQLAlchemy name is not allowed to declare a node.
	for _, name := range ents.order {
		m := ents.table[name]
		if !m.certain {
			continue
		}
		st, ok := emit("sqlalchemy", "sqlalchemy.__tablename__", AccessDDL, ents.line[name], true, m.table)
		if !ok {
			continue
		}
		st.Columns = ents.columns[name]
		out = append(out, st)
	}

	for _, c := range chainsIn(toks, match) {
		st, ok := pyChainAccess(c, ents, vars, handles)
		if ok {
			out = append(out, st)
		}
	}
	return out
}

func usesSQLAlchemy(toks []token) bool {
	for i := 0; i+1 < len(toks); i++ {
		if toks[i].kind != tokIdent {
			continue
		}
		if toks[i].text != "import" && toks[i].text != "from" {
			continue
		}
		switch toks[i+1].text {
		case "sqlalchemy", "flask_sqlalchemy", "sqlalchemy_utils":
			return true
		}
	}
	return false
}

// pyEntityIndex is the file's declared tables, in source order.
type pyEntityIndex struct {
	order   []string
	table   map[string]mapping
	line    map[string]int
	columns map[string][]string
}

func (e pyEntityIndex) lookup(name string) (mapping, bool) {
	m, ok := e.table[name]
	return m, ok
}

// pyEntities reads declarative classes and Core Table() constructions.
func pyEntities(toks []token, match []int) pyEntityIndex {
	idx := pyEntityIndex{
		table:   map[string]mapping{},
		line:    map[string]int{},
		columns: map[string][]string{},
	}
	record := func(name string, m mapping, line int, cols []string) {
		if name == "" || m.table == "" {
			return
		}
		if _, dup := idx.table[name]; !dup {
			idx.order = append(idx.order, name)
		}
		idx.table[name] = m
		idx.line[name] = line
		idx.columns[name] = dedupeLower(cols)
	}

	for i := 0; i+1 < len(toks); i++ {
		t := toks[i]

		// Core: `orders = Table("orders", metadata, Column("id", Integer))`.
		if t.kind == tokIdent && i+3 < len(toks) && isPunct(toks[i+1], "=") &&
			toks[i+2].kind == tokIdent && toks[i+2].text == "Table" &&
			isPunct(toks[i+3], "(") && match[i+3] > 0 {
			args := toks[i+4 : match[i+3]]
			if name, ok := firstString(args); ok && isIdentifier(name) {
				record(t.text, mapping{table: unqualify(name), certain: true}, t.line,
					pyColumnArgs(toks, match, i+4, match[i+3]))
			}
			continue
		}

		// Declarative: `class Order(Base):`.
		if t.kind != tokIdent || t.text != "class" || toks[i+1].kind != tokIdent {
			continue
		}
		name := toks[i+1].text
		bodyStart := i + 2
		flask := false
		if bodyStart < len(toks) && isPunct(toks[bodyStart], "(") && match[bodyStart] > 0 {
			bases := toks[bodyStart+1 : match[bodyStart]]
			flask = pyNamesFlaskModel(bases)
			bodyStart = match[bodyStart] + 1
		}
		end := pySuiteEnd(toks, bodyStart, t.line, t.col)

		m := mapping{}
		if s := pyClassString(toks, bodyStart, end, "__tablename__"); s != "" && isIdentifier(s) {
			m = mapping{table: unqualify(s), certain: true}
		} else if s := pyDunderTable(toks, match, bodyStart, end); s != "" && isIdentifier(s) {
			m = mapping{table: unqualify(s), certain: true}
		} else if flask {
			// Flask-SQLAlchemy's documented default for db.Model subclasses.
			m = mapping{table: snakeCase(name), certain: false}
		}
		record(name, m, t.line, pyMappedColumns(toks, match, bodyStart, end))
	}
	return idx
}

func pyNamesFlaskModel(bases []token) bool {
	for i := range bases {
		if bases[i].kind == tokIdent && bases[i].text == "Model" {
			return true
		}
	}
	return false
}

// pySuiteEnd returns the end of a block opened on the given line at the given
// column: the first token on a later line that is not indented past it. This is
// Python's block structure read directly, which is why the lexer carries a
// column at all.
func pySuiteEnd(toks []token, from, line, col int) int {
	for i := from; i < len(toks); i++ {
		if toks[i].line > line && toks[i].col <= col {
			return i
		}
	}
	return len(toks)
}

// pyClassString reads `NAME = "value"` from a class body.
func pyClassString(toks []token, from, to int, name string) string {
	for i := from; i+2 < to && i+2 < len(toks); i++ {
		if toks[i].kind == tokIdent && toks[i].text == name &&
			isPunct(toks[i+1], "=") && toks[i+2].kind == tokString {
			return strings.TrimSpace(toks[i+2].text)
		}
	}
	return ""
}

// pyDunderTable reads `__table__ = Table("orders", ...)`.
func pyDunderTable(toks []token, match []int, from, to int) string {
	for i := from; i+3 < to && i+3 < len(toks); i++ {
		if toks[i].kind != tokIdent || toks[i].text != "__table__" {
			continue
		}
		if !isPunct(toks[i+1], "=") || toks[i+2].kind != tokIdent || toks[i+2].text != "Table" {
			continue
		}
		if !isPunct(toks[i+3], "(") || match[i+3] < 0 {
			continue
		}
		if s, ok := firstString(toks[i+4 : match[i+3]]); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// pyMappedColumns reads a declarative class's mapped attributes:
// `id = Column(...)`, `id: Mapped[int] = mapped_column(...)`, and the explicit
// `Column("db_name", ...)` spelling where the attribute and the column differ.
func pyMappedColumns(toks []token, match []int, from, to int) []string {
	var cols []string
	for i := from; i+2 < to && i+2 < len(toks); i++ {
		if toks[i].kind != tokIdent || !isPunct(toks[i+1], "=") {
			continue
		}
		j := i + 2
		if toks[j].kind != tokIdent || (toks[j].text != "Column" && toks[j].text != "mapped_column") {
			continue
		}
		if j+1 >= len(toks) || !isPunct(toks[j+1], "(") || match[j+1] < 0 {
			continue
		}
		args := toks[j+2 : match[j+1]]
		if s, ok := firstString(args); ok && isIdentifier(s) && len(args) > 0 && args[0].kind == tokString {
			cols = append(cols, unqualify(s))
			continue
		}
		if isIdentifier(toks[i].text) {
			cols = append(cols, unqualify(toks[i].text))
		}
	}
	return cols
}

// pyColumnArgs reads `Column("name", ...)` out of a Table() argument list.
//
// Absolute token indices throughout. An earlier version took a subslice and an
// offset, and computed one bound from the other; the fuzzer found the input
// where they disagreed, which is the argument for not carrying two coordinate
// systems at once.
func pyColumnArgs(toks []token, match []int, from, to int) []string {
	var cols []string
	for i := from; i+1 < to && i+1 < len(toks); i++ {
		if toks[i].kind != tokIdent || toks[i].text != "Column" || !isPunct(toks[i+1], "(") {
			continue
		}
		end := match[i+1]
		if end < 0 || end > to {
			continue
		}
		if s, ok := firstString(toks[i+2 : end]); ok && isIdentifier(s) {
			cols = append(cols, unqualify(s))
		}
		i = end
	}
	return cols
}

// pyVarTypes maps an identifier to the class it was constructed from.
//
// Python states no types, so this reads the one form that carries one anyway: a
// constructor call, `order = Order(total=10)`. `order = fetch()` states nothing
// and is not guessed at. The annotated form `order: Order = ...` is read too,
// because an annotation is a statement of type rather than an inference from
// one.
func pyVarTypes(toks []token) map[string]string {
	out := make(map[string]string)
	for i := 0; i+2 < len(toks); i++ {
		if toks[i].kind != tokIdent || isTypeName(toks[i].text) {
			continue
		}
		switch {
		case isPunct(toks[i+1], "=") && toks[i+2].kind == tokIdent &&
			isTypeName(toks[i+2].text) && i+3 < len(toks) && isPunct(toks[i+3], "("):
			out[toks[i].text] = toks[i+2].text
		case isPunct(toks[i+1], ":") && toks[i+2].kind == tokIdent && isTypeName(toks[i+2].text):
			out[toks[i].text] = toks[i+2].text
		}
	}
	return out
}

// pySessionHandles collects identifiers bound to a Session.
//
// `session` itself is accepted as a handle name because it is SQLAlchemy's own
// documented spelling for the object and `db.session` is Flask-SQLAlchemy's —
// but only inside a file that already imports SQLAlchemy, which is what keeps
// an unrelated `session.get(...)` on an HTTP client out of the graph.
func pySessionHandles(toks []token) map[string]bool {
	out := map[string]bool{"session": true, "Session": true, "db_session": true}
	for i := 0; i+2 < len(toks); i++ {
		if toks[i].kind != tokIdent || !isPunct(toks[i+1], "=") || toks[i+2].kind != tokIdent {
			continue
		}
		if pySessionFactories[toks[i+2].text] {
			out[toks[i].text] = true
		}
	}
	return out
}

// pyChainAccess classifies one chain.
func pyChainAccess(c chain, ents pyEntityIndex, vars map[string]string, handles map[string]bool) (ORMStatement, bool) {
	// A Core constructor: `select(Order)`, `insert(orders)`. Recognised on its
	// own, because it is a statement in its own right and is usually handed to
	// `session.execute(...)` where the outer chain names no entity.
	if len(c.segs) >= 1 && c.segs[0].called {
		if access, ok := pyConstructors[c.segs[0].name]; ok {
			if m, ok := pyEntityFrom(c.segs[0].args, ents, vars); ok {
				return mustEmit("sqlalchemy", "sqlalchemy."+c.segs[0].name, pyTerminalOverride(c, access), c.line, m)
			}
		}
	}

	onSession := false
	for _, s := range c.segs {
		if !s.called && handles[s.name] {
			onSession = true
		}
	}

	// `Order.query.filter_by(...).all()` — Flask-SQLAlchemy's spelling, where
	// the entity is the chain's own root.
	if len(c.segs) >= 2 && !c.segs[0].called && c.segs[1].name == "query" && !c.segs[1].called {
		if m, ok := ents.lookup(c.segs[0].name); ok {
			return mustEmit("sqlalchemy", "sqlalchemy.query", pyTerminalOverride(c, AccessRead), c.line, m)
		}
	}

	for _, s := range c.segs {
		if !s.called {
			continue
		}
		var access Access
		switch {
		case pyRead[s.name]:
			if !onSession {
				continue
			}
			access = AccessRead
		case pyWrite[s.name]:
			if !onSession {
				continue
			}
			access = AccessWrite
		default:
			continue
		}
		if m, ok := pyEntityFrom(s.args, ents, vars); ok {
			return mustEmit("sqlalchemy", "sqlalchemy."+s.name, pyTerminalOverride(c, access), c.line, m)
		}
	}
	return ORMStatement{}, false
}

// pyTerminalOverride upgrades a read to a write when the chain ends in one:
// `session.query(Order).filter(...).delete()` is a delete, and reporting it as
// a read would hide the only write in the file.
func pyTerminalOverride(c chain, base Access) Access {
	for _, s := range c.segs {
		if !s.called {
			continue
		}
		if s.name == "delete" || s.name == "update" {
			return AccessWrite
		}
	}
	return base
}

// pyEntityFrom resolves an argument list to a declared table.
func pyEntityFrom(args []token, ents pyEntityIndex, vars map[string]string) (mapping, bool) {
	for _, t := range args {
		if t.kind != tokIdent {
			continue
		}
		if m, ok := ents.lookup(t.text); ok {
			return m, true
		}
		if ty, ok := vars[t.text]; ok {
			if m, ok := ents.lookup(ty); ok {
				return m, true
			}
		}
	}
	return mapping{}, false
}

func mustEmit(framework, verb string, access Access, line int, m mapping) (ORMStatement, bool) {
	return emit(framework, verb, access, line, m.certain, m.table)
}
