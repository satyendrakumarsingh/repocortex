package db

import "strings"

// JPA / Hibernate / Spring Data JPA.
//
// # Three declarations, three kinds of access
//
// Java spreads one table's story across three shapes, and all three are read
// here because any one alone answers half a question:
//
//   - `@Entity class Order` with `@Table(name = "orders")` DECLARES the table,
//     and `@Column(name = ...)` declares its columns.
//   - `interface OrderRepository extends JpaRepository<Order, Long>` declares
//     an access surface. Extending CrudRepository is not a hint that CRUD
//     happens — it is the declaration of the CRUD methods themselves — and each
//     method the interface adds (`findByStatus`, `deleteByCreatedAtBefore`)
//     states its own kind in its own name, which is the whole point of Spring
//     Data's derived-query naming.
//   - `entityManager.persist(order)` / `session.get(Order.class, id)` is the
//     operation written out longhand, and it names its entity directly.
//
// # The gate
//
// The file must carry JPA's own spelling: a persistence annotation, a
// `javax.`/`jakarta.persistence` or `org.hibernate` import, a Spring Data
// repository base, or the `EntityManager` type. And an EntityManager call must
// hang off a value the file DECLARES as one of those types — `save(x)` on an
// arbitrary receiver is a method name, not a fact about a database.
//
// # The default table name, stated exactly
//
// With no `@Table(name=...)`, two conventions are in play and they disagree.
// Spring Boot installs CamelCaseToUnderscoresNamingStrategy as the physical
// naming strategy, giving `OrderItem` -> `order_item`. Plain Hibernate's
// PhysicalNamingStrategyStandardImpl passes the JPA implicit name through
// unchanged, giving `OrderItem` -> `OrderItem`, which this package lowercases
// to `orderitem`. They agree exactly when the entity name has no internal
// camel-case boundary. So: a single-word entity is reported certain, a
// multi-word one is reported with the Spring Boot spelling and certain=false.

// jpaRepoBases maps a Spring Data base interface to the access its inherited
// methods perform. The marker `Repository<T, ID>` declares no methods and so
// declares no access — it appears here with an empty mode and is used only to
// learn the entity for the methods the interface itself adds.
var jpaRepoBases = map[string]Access{
	"CrudRepository":                 AccessReadWrite,
	"ListCrudRepository":             AccessReadWrite,
	"JpaRepository":                  AccessReadWrite,
	"PagingAndSortingRepository":     AccessRead,
	"ListPagingAndSortingRepository": AccessRead,
	"JpaSpecificationExecutor":       AccessRead,
	"QueryByExampleExecutor":         AccessRead,
	"QuerydslPredicateExecutor":      AccessRead,
	"Repository":                     "",
}

// jpaReadPrefixes and jpaWritePrefixes are Spring Data's derived-query subject
// keywords. The prefix IS the operation — `deleteByStatus` deletes — so this is
// reading a declaration, not inferring from a name.
var jpaReadPrefixes = []string{"findBy", "findAll", "findFirst", "findTop", "find",
	"readBy", "read", "getBy", "get", "queryBy", "query", "searchBy", "search",
	"streamBy", "stream", "countBy", "count", "existsBy", "exists"}

var jpaWritePrefixes = []string{"deleteBy", "deleteAll", "delete", "removeBy", "remove",
	"saveAll", "saveAnd", "save", "insert", "update", "persist"}

// jpaEMRead and jpaEMWrite are the EntityManager and Hibernate Session
// operations. Split by what the operation does, never by where it appears.
var jpaEMRead = map[string]bool{
	"find": true, "getReference": true, "get": true, "load": true,
	"byId": true, "byMultipleIds": true, "refresh": true,
	"createQuery": true, "createNamedQuery": true, "createNativeQuery": true,
}

var jpaEMWrite = map[string]bool{
	"persist": true, "merge": true, "remove": true, "save": true,
	"saveOrUpdate": true, "update": true, "delete": true, "replicate": true,
	"persistAndFlush": true,
}

// jpaHandleTypes are the declared types a persistence call may hang off.
var jpaHandleTypes = map[string]bool{
	"EntityManager": true, "Session": true, "StatelessSession": true,
	"HibernateTemplate": true, "JpaTemplate": true,
}

func jpaStatements(src string) []ORMStatement {
	toks := lexBraced(src)
	if !usesJPA(toks) {
		return nil
	}
	match := matchIndex(toks)

	ents := jpaEntities(toks, match)
	vars := javaVarTypes(toks)

	var out []ORMStatement

	// The entity declaration. A table node is claimed here only when the name
	// is certain — an `@Table(name=...)`, or a single-word entity where both
	// naming strategies agree. A conventional guess at a compound name would
	// put a node in the graph under a name no database necessarily has, and a
	// wrong node is worse than an unbound access.
	for _, e := range ents.order {
		m := ents.table[e]
		if !m.certain {
			continue
		}
		st, ok := emit("jpa", "jpa.@Entity", AccessDDL, ents.line[e], true, m.table)
		if !ok {
			continue
		}
		if e == ents.first {
			st.Columns = ents.columns
		}
		out = append(out, st)
	}

	out = append(out, jpaRepositoryAccesses(toks, match, ents)...)
	out = append(out, jpaSessionAccesses(toks, match, ents, vars)...)
	return out
}

func usesJPA(toks []token) bool {
	for i, t := range toks {
		if t.kind != tokIdent {
			continue
		}
		switch t.text {
		case "persistence", "hibernate", "EntityManager", "PersistenceContext":
			return true
		}
		if _, ok := jpaRepoBases[t.text]; ok && t.text != "Repository" {
			return true
		}
		if i > 0 && isPunct(toks[i-1], "@") {
			switch t.text {
			case "Entity", "Table", "MappedSuperclass", "Embeddable", "Column":
				return true
			}
		}
	}
	return false
}

// jpaEntityIndex is a file's entity declarations, in source order.
type jpaEntityIndex struct {
	order []string
	table map[string]mapping
	line  map[string]int
	// first is the file's first @Entity class. Java puts one public type in a
	// file, so the `@Column` annotations found anywhere in it belong to that
	// entity; attributing them per nested class would be precision nobody can
	// use.
	first   string
	columns []string
}

func (e jpaEntityIndex) tableFor(typeName string) mapping {
	if m, ok := e.table[typeName]; ok {
		return m
	}
	return jpaDefaultTable(typeName)
}

// jpaDefaultTable applies Spring Boot's CamelCaseToUnderscoresNamingStrategy,
// and reports certainty only when plain Hibernate would agree.
func jpaDefaultTable(typeName string) mapping {
	if typeName == "" {
		return mapping{}
	}
	return mapping{table: snakeCase(typeName), certain: !isCompound(typeName)}
}

// jpaEntities reads `@Entity`, `@Table(name=...)` and `@Column(name=...)`.
//
// Annotations are accumulated as they are seen and consumed by the declaration
// that follows, which is what an annotation IS — so `@Entity @Table(name="x")
// public class Order` binds the table to Order without the scan needing to know
// which modifiers may sit between them.
func jpaEntities(toks []token, match []int) jpaEntityIndex {
	idx := jpaEntityIndex{table: map[string]mapping{}, line: map[string]int{}}
	type anno struct {
		name string
		args []token
	}
	var pending []anno

	has := func(name string) (anno, bool) {
		for _, a := range pending {
			if a.name == name {
				return a, true
			}
		}
		return anno{}, false
	}

	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if isPunct(t, "@") && i+1 < len(toks) && toks[i+1].kind == tokIdent {
			a := anno{name: toks[i+1].text}
			i++
			if i+1 < len(toks) && isPunct(toks[i+1], "(") && match[i+1] >= 0 {
				a.args = toks[i+2 : match[i+1]]
				i = match[i+1]
			}
			pending = append(pending, a)
			continue
		}
		if t.kind == tokIdent && (t.text == "class" || t.text == "interface" || t.text == "enum") &&
			i+1 < len(toks) && toks[i+1].kind == tokIdent {
			name := toks[i+1].text
			if _, isEntity := has("Entity"); isEntity {
				m := jpaDefaultTable(name)
				if tbl, ok := has("Table"); ok {
					if n, ok := namedString(tbl.args, "name"); ok && isIdentifier(n) {
						m = mapping{table: unqualify(n), certain: true}
					}
				}
				if _, dup := idx.table[name]; !dup {
					idx.order = append(idx.order, name)
				}
				idx.table[name] = m
				idx.line[name] = toks[i+1].line
				if idx.first == "" {
					idx.first = name
				}
			}
			pending = nil
			continue
		}
		if isPunct(t, ";") || isPunct(t, "{") || isPunct(t, "}") {
			// A field or method declaration ends here; a `@Column(name=...)`
			// that was waiting for one names a column.
			for _, a := range pending {
				if a.name != "Column" && a.name != "JoinColumn" {
					continue
				}
				if n, ok := namedString(a.args, "name"); ok && isIdentifier(n) {
					idx.columns = append(idx.columns, unqualify(n))
				}
			}
			pending = nil
		}
	}
	idx.columns = dedupeLower(idx.columns)
	return idx
}

// jpaRepositoryAccesses reads Spring Data repository interfaces.
func jpaRepositoryAccesses(toks []token, match []int, ents jpaEntityIndex) []ORMStatement {
	var out []ORMStatement
	for i := 0; i+1 < len(toks); i++ {
		if toks[i].kind != tokIdent || toks[i].text != "interface" || toks[i+1].kind != tokIdent {
			continue
		}
		// Header runs from the name to the body's opening brace.
		body := i + 2
		for ; body < len(toks) && !isPunct(toks[body], "{"); body++ {
		}
		entity, base := "", ""
		for j := i + 2; j < body; j++ {
			if toks[j].kind != tokIdent {
				continue
			}
			mode, isBase := jpaRepoBases[toks[j].text]
			if !isBase {
				continue
			}
			ent := javaFirstTypeArg(toks, j+1, body)
			if ent == "" {
				continue
			}
			entity, base = ent, toks[j].text
			if mode != "" {
				m := ents.tableFor(ent)
				if st, ok := emit("jpa", "jpa."+toks[j].text, mode, toks[i].line, m.certain, m.table); ok {
					out = append(out, st)
				}
			}
		}
		if entity == "" || base == "" || body >= len(toks) || match[body] < 0 {
			continue
		}
		m := ents.tableFor(entity)
		for _, meth := range javaDeclaredMethods(toks, match, body+1, match[body]) {
			access := jpaMethodAccess(meth.name)
			if access == "" {
				continue
			}
			if st, ok := emit("jpa", "jpa."+meth.name, access, meth.line, m.certain, m.table); ok {
				out = append(out, st)
			}
		}
	}
	return out
}

// jpaMethodAccess classifies a derived query method by its subject keyword.
func jpaMethodAccess(name string) Access {
	for _, p := range jpaWritePrefixes {
		if strings.HasPrefix(name, p) {
			return AccessWrite
		}
	}
	for _, p := range jpaReadPrefixes {
		if strings.HasPrefix(name, p) {
			return AccessRead
		}
	}
	return ""
}

type javaMethod struct {
	name string
	line int
}

// javaDeclaredMethods finds the methods declared directly in an interface body:
// an identifier immediately before a parenthesis at the body's own nesting
// level. Nested types' methods are skipped, which is what "directly" means.
func javaDeclaredMethods(toks []token, match []int, from, to int) []javaMethod {
	var out []javaMethod
	for i := from; i < to && i < len(toks); i++ {
		if isPunct(toks[i], "{") && match[i] > 0 {
			i = match[i]
			continue
		}
		if !isPunct(toks[i], "(") || match[i] < 0 {
			continue
		}
		if i == from || toks[i-1].kind != tokIdent {
			i = match[i]
			continue
		}
		out = append(out, javaMethod{name: toks[i-1].text, line: toks[i-1].line})
		i = match[i]
	}
	return out
}

// javaFirstTypeArg reads `<Order, Long>` and returns `Order`, tolerating a
// qualified name by taking the last identifier of the first argument.
func javaFirstTypeArg(toks []token, from, to int) string {
	if from >= len(toks) || !isPunct(toks[from], "<") {
		return ""
	}
	last := ""
	for i := from + 1; i < to && i < len(toks); i++ {
		if isPunct(toks[i], ",") || isPunct(toks[i], ">") {
			break
		}
		if toks[i].kind == tokIdent {
			last = toks[i].text
		}
	}
	if isTypeName(last) {
		return last
	}
	return ""
}

// jpaSessionAccesses reads EntityManager and Hibernate Session calls.
func jpaSessionAccesses(toks []token, match []int, ents jpaEntityIndex, vars map[string]string) []ORMStatement {
	var out []ORMStatement
	for _, c := range chainsIn(toks, match) {
		if !jpaChainQualifies(c, vars) {
			continue
		}
		for _, s := range c.segs {
			if !s.called {
				continue
			}
			var access Access
			switch {
			case jpaEMWrite[s.name]:
				access = AccessWrite
			case jpaEMRead[s.name]:
				access = AccessRead
			default:
				continue
			}
			entity := javaEntityArg(s.args, vars)
			if entity == "" {
				continue
			}
			m := ents.tableFor(entity)
			if st, ok := emit("jpa", "jpa."+s.name, access, c.line, m.certain, m.table); ok {
				out = append(out, st)
			}
		}
	}
	return out
}

// jpaChainQualifies requires the receiver to be DECLARED as a persistence
// handle somewhere in the file. A name is not evidence; a type is.
func jpaChainQualifies(c chain, vars map[string]string) bool {
	for _, s := range c.segs {
		if s.called {
			continue
		}
		if jpaHandleTypes[vars[s.name]] {
			return true
		}
	}
	return false
}

// javaEntityArg finds the entity an operation was handed: a class literal
// (`Order.class`), or an argument whose declared type is a class.
func javaEntityArg(args []token, vars map[string]string) string {
	for i := 0; i+2 < len(args); i++ {
		if args[i].kind == tokIdent && isPunct(args[i+1], ".") &&
			args[i+2].kind == tokIdent && args[i+2].text == "class" &&
			isTypeName(args[i].text) {
			return args[i].text
		}
	}
	for _, t := range args {
		if t.kind != tokIdent {
			continue
		}
		if ty, ok := vars[t.text]; ok && isTypeName(ty) && !jpaHandleTypes[ty] {
			return ty
		}
	}
	return ""
}

// javaVarTypes maps an identifier to its declared type.
//
// Java states the type at every declaration site — field, parameter, local — so
// this reads declarations rather than inferring from initialisers. `List<Order>
// orders` resolves to `Order`, because the element type is the one that names a
// table.
func javaVarTypes(toks []token) map[string]string {
	out := make(map[string]string)
	for i := 1; i+1 < len(toks); i++ {
		name := toks[i]
		if name.kind != tokIdent || isTypeName(name.text) {
			continue
		}
		next := toks[i+1]
		if !isPunct(next, "=") && !isPunct(next, ";") && !isPunct(next, ",") && !isPunct(next, ")") {
			continue
		}
		prev := toks[i-1]
		switch {
		case prev.kind == tokIdent && isTypeName(prev.text):
			out[name.text] = prev.text
		case isPunct(prev, ">"):
			// A generic: walk back to the opening angle bracket and take the
			// last type named inside it.
			last := ""
			for j := i - 2; j >= 0 && j > i-16; j-- {
				if isPunct(toks[j], "<") {
					break
				}
				if toks[j].kind == tokIdent && isTypeName(toks[j].text) {
					last = toks[j].text
				}
			}
			if last != "" {
				out[name.text] = last
			}
		}
	}
	return out
}
