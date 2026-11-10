package db

import "strings"

// GORM (gorm.io/gorm).
//
// # The gate
//
// Two conditions, both required, before a single access is reported. The FILE
// must name GORM — the import path `gorm.io/gorm`, its predecessor
// `github.com/jinzhu/gorm`, or a `gorm:"..."` struct tag. And the CHAIN must
// either hang off a value declared as `*gorm.DB` or call a method that is
// GORM's own vocabulary (`Model`, `Table`, `Preload`, `Migrator`, ...).
//
// Neither is optional. `Delete`, `Save`, `Find` and `Count` are words that
// appear on caches, slices, wrappers and test doubles; a scan that reported
// them because the file also happens to import GORM would put a table behind
// every method call in the file.
//
// # What a table name is derived from
//
//  1. `Table("orders")` — stated outright.
//  2. `TableName() string { return "orders" }` on the entity — also stated.
//  3. `Model(&Order{})`, `Create(&Order{})`, `AutoMigrate(&Order{})` — the
//     entity type, mapped by GORM's default NamingStrategy: snake_case, then
//     pluralised. `OrderItem` -> `order_items`. Not certain: `SingularTable`
//     is a one-line configuration change.
//  4. `Find(&orders)` — the variable's declared element type, when the file
//     declares it. Not certain, because the type map here is file-scoped
//     rather than block-scoped.
//
// A chain that resolves to none of these is dropped. `db.Find(&rows)` where
// `rows` is declared in another file names no table, and turning `rows` into
// one would be the fabrication the SQL side's whole false-positive gate exists
// to prevent.

// gormWrite are the operations that modify rows. `Save` is here rather than in
// read-write because GORM's Save is an upsert of the value it was handed, not a
// read-then-write of other rows.
var gormWrite = map[string]bool{
	"Create": true, "CreateInBatches": true, "Save": true, "Update": true,
	"Updates": true, "UpdateColumn": true, "UpdateColumns": true,
	"Delete": true,
}

var gormRead = map[string]bool{
	"First": true, "Last": true, "Take": true, "Find": true,
	"FindInBatches": true, "Scan": true, "Count": true, "Pluck": true,
	"Row": true, "Rows": true, "FirstOrInit": true,
}

// gormReadWrite is the one operation that provably does both in a single call:
// FirstOrCreate looks the row up and inserts it when it is missing.
var gormReadWrite = map[string]bool{"FirstOrCreate": true}

// gormDDL are Migrator operations and AutoMigrate, which change the schema
// rather than the data — the distinction migration impact turns on.
var gormDDL = map[string]bool{
	"AutoMigrate": true, "CreateTable": true, "DropTable": true,
	"RenameTable": true, "AddColumn": true, "DropColumn": true,
	"AlterColumn": true, "RenameColumn": true, "CreateIndex": true,
	"DropIndex": true, "CreateConstraint": true, "DropConstraint": true,
}

// gormVocabulary is the framework-spelling half of the gate. Every entry is a
// method GORM defines and ordinary Go code does not casually spell.
var gormVocabulary = map[string]bool{
	"Model": true, "Table": true, "Preload": true, "Joins": true,
	"Migrator": true, "AutoMigrate": true, "Unscoped": true, "Clauses": true,
	"Scopes": true, "Omit": true, "FirstOrCreate": true, "FirstOrInit": true,
	"WithContext": true, "Association": true, "Attrs": true, "Assign": true,
}

// gormTakesEntityArgs are the operations whose arguments name the entity
// directly, so an argument scan is reading a declaration rather than guessing.
var gormTakesEntityArgs = map[string]bool{
	"Model": true, "AutoMigrate": true, "CreateTable": true, "DropTable": true,
	"Create": true, "CreateInBatches": true, "Save": true, "Delete": true,
	"First": true, "Last": true, "Take": true, "Find": true,
	"FindInBatches": true, "Scan": true, "FirstOrCreate": true,
	"FirstOrInit": true, "Migrator": true, "Updates": true,
}

func gormStatements(src string) []ORMStatement {
	toks := lexBraced(src)
	if !usesGORM(toks) {
		return nil
	}
	match := matchIndex(toks)

	overrides := gormTableNameMethods(toks, match)
	handles := gormHandles(toks)
	vars := goVarTypes(toks)

	var out []ORMStatement

	// An explicit TableName() states the table outright, which makes the
	// declaring file the table's home — the same reason a CREATE TABLE
	// declares the node rather than a query against it.
	for _, d := range overrides.order {
		if st, ok := emit("gorm", "gorm.TableName", AccessDDL, overrides.line[d], true, overrides.table[d]); ok {
			out = append(out, st)
		}
	}

	for _, c := range chainsIn(toks, match) {
		if !gormChainQualifies(c, handles) {
			continue
		}
		op, access := gormOperation(c)
		if access == "" {
			continue
		}
		tables, certain := gormTables(c, overrides, vars)
		if len(tables) == 0 {
			continue
		}
		if st, ok := emit("gorm", "gorm."+op, access, c.line, certain, tables...); ok {
			out = append(out, st)
		}
	}
	return out
}

// usesGORM is the file-level half of the gate.
func usesGORM(toks []token) bool {
	for _, t := range toks {
		if t.kind != tokString {
			continue
		}
		if t.text == "gorm.io/gorm" || strings.HasPrefix(t.text, "gorm.io/") ||
			strings.HasSuffix(t.text, "jinzhu/gorm") || strings.Contains(t.text, `gorm:"`) {
			return true
		}
	}
	return false
}

// gormChainQualifies is the chain-level half of the gate.
func gormChainQualifies(c chain, handles map[string]bool) bool {
	for _, s := range c.segs {
		if !s.called && handles[s.name] {
			return true
		}
	}
	return c.hasCall(gormVocabulary)
}

// gormHandles collects identifiers declared as `*gorm.DB`, wherever the
// declaration appears: a struct field, a parameter, a var, a return type.
//
// The scan is for the TYPE, not for a name. `db` is a bare name that means
// anything; `*gorm.DB` is GORM saying so.
func gormHandles(toks []token) map[string]bool {
	out := make(map[string]bool)
	for i := 0; i+2 < len(toks); i++ {
		if toks[i].kind != tokIdent || toks[i].text != "gorm" {
			continue
		}
		if !isPunct(toks[i+1], ".") || toks[i+2].kind != tokIdent || toks[i+2].text != "DB" {
			continue
		}
		// Walk back over the pointer star and any `var`/`(`/`,` to the name.
		j := i - 1
		if j >= 0 && isPunct(toks[j], "*") {
			j--
		}
		if j >= 0 && toks[j].kind == tokIdent && toks[j].text != "var" && toks[j].text != "func" {
			out[toks[j].text] = true
		}
	}
	return out
}

// gormOperation picks the operation a chain performs and the access it implies.
//
// The LAST recognised operation wins, because a chain is read left to right and
// its terminal call is what runs — `db.Where(...).Delete(&Order{})` is a delete,
// and `Where` classifies nothing on its own. DDL outranks the rest so
// `db.Migrator().CreateTable(&Order{})` is not read as a create of rows.
func gormOperation(c chain) (string, Access) {
	op, access := "", Access("")
	for _, s := range c.segs {
		if !s.called {
			continue
		}
		switch {
		case gormDDL[s.name]:
			return s.name, AccessDDL
		case gormReadWrite[s.name]:
			op, access = s.name, AccessReadWrite
		case gormWrite[s.name]:
			op, access = s.name, AccessWrite
		case gormRead[s.name]:
			op, access = s.name, AccessRead
		}
	}
	return op, access
}

// gormTables resolves the table or tables a chain targets.
func gormTables(c chain, overrides gormOverrides, vars map[string]string) ([]string, bool) {
	// 1. Stated outright. `Table("orders o")` carries an alias; the table is
	//    the first field, and nothing else in that string is a name.
	if seg, ok := c.call("Table"); ok {
		if name, ok := firstString(seg.args); ok {
			if f := strings.Fields(name); len(f) > 0 && isIdentifier(f[0]) {
				return []string{unqualify(f[0])}, true
			}
		}
	}

	// 2. The entity, from whichever argument list names one. Model first: when
	//    a chain has both, Model is the statement's subject and the terminal
	//    argument is only where the result lands.
	var types []string
	if seg, ok := c.call("Model"); ok {
		types = entityTypesIn(seg.args)
	}
	if len(types) == 0 {
		for _, s := range c.segs {
			if s.called && gormTakesEntityArgs[s.name] {
				if t := entityTypesIn(s.args); len(t) > 0 {
					types = append(types, t...)
				}
			}
		}
	}
	var tables []string
	for _, ty := range types {
		if m, ok := overrides.table[ty]; ok {
			tables = append(tables, m)
			continue
		}
		tables = append(tables, gormDefaultTable(ty))
	}
	if len(tables) > 0 {
		// A chain naming several entities (AutoMigrate) is certain only if
		// every one of them stated its table.
		return tables, allOverridden(types, overrides)
	}

	// 3. The destination variable's element type, when this file declares it.
	//    File-scoped rather than block-scoped, hence never certain.
	for _, s := range c.segs {
		if !s.called || !gormTakesEntityArgs[s.name] {
			continue
		}
		for _, t := range s.args {
			if t.kind != tokIdent {
				continue
			}
			if ty, ok := vars[t.text]; ok {
				if m, ok := overrides.table[ty]; ok {
					return []string{m}, false
				}
				return []string{gormDefaultTable(ty)}, false
			}
		}
	}
	return nil, false
}

func allOverridden(types []string, overrides gormOverrides) bool {
	for _, ty := range types {
		if _, ok := overrides.table[ty]; !ok {
			return false
		}
	}
	return len(types) > 0
}

// gormDefaultTable is GORM's documented default NamingStrategy: the struct name
// snake_cased and pluralised.
func gormDefaultTable(typeName string) string {
	return pluralise(snakeCase(typeName))
}

// entityTypesIn finds composite-literal type names in an argument list:
// `&Order{}`, `Order{}`, `[]Order{}`, `&[]Order{}`, `&pkg.Order{}`.
//
// The literal's brace is what makes this structural rather than a guess — an
// identifier followed by `{` in an argument position is a composite literal and
// nothing else.
func entityTypesIn(args []token) []string {
	var out []string
	for i := 0; i+1 < len(args); i++ {
		if args[i].kind != tokIdent || !isPunct(args[i+1], "{") {
			continue
		}
		if !isTypeName(args[i].text) {
			continue
		}
		out = append(out, args[i].text)
	}
	return out
}

// gormOverrides is the entity-to-table map a file states through TableName
// methods, kept in declaration order so output does not depend on map
// iteration.
type gormOverrides struct {
	order []string
	table map[string]string
	line  map[string]int
}

// gormTableNameMethods reads `func (o Order) TableName() string { return "..." }`.
//
// Matched on the method's full shape — receiver, the name TableName, an empty
// parameter list — rather than on the word, so a field or a local called
// TableName is not mistaken for the declaration.
func gormTableNameMethods(toks []token, match []int) gormOverrides {
	ov := gormOverrides{table: map[string]string{}, line: map[string]int{}}
	for i := 0; i+3 < len(toks); i++ {
		if toks[i].kind != tokIdent || toks[i].text != "TableName" {
			continue
		}
		if !isPunct(toks[i+1], "(") || !isPunct(toks[i+2], ")") {
			continue
		}
		if i == 0 || !isPunct(toks[i-1], ")") {
			continue
		}
		open := match[i-1]
		if open < 0 || open+1 >= i-1 {
			continue
		}
		// The receiver type is the last identifier in `(o *Order)`.
		recv := ""
		for j := open + 1; j < i-1; j++ {
			if toks[j].kind == tokIdent {
				recv = toks[j].text
			}
		}
		if !isTypeName(recv) {
			continue
		}
		// The returned literal, within the method body.
		body := i + 3
		for ; body < len(toks) && !isPunct(toks[body], "{"); body++ {
		}
		end := len(toks)
		if body < len(toks) && match[body] >= 0 {
			end = match[body]
		}
		for j := body; j < end; j++ {
			if toks[j].kind != tokString {
				continue
			}
			name := strings.TrimSpace(toks[j].text)
			if name == "" || !isIdentifier(name) {
				break
			}
			if _, dup := ov.table[recv]; !dup {
				ov.order = append(ov.order, recv)
			}
			ov.table[recv] = unqualify(name)
			ov.line[recv] = toks[i].line
			break
		}
	}
	return ov
}

// goVarTypes maps a variable to the entity type it holds, for the declaration
// forms that state a type outright.
//
// Only those forms. `u := repo.Load()` states no type and is not guessed at;
// treating `Load` as an entity would put a table called `loads` in the graph.
func goVarTypes(toks []token) map[string]string {
	out := make(map[string]string)
	for i := 1; i+1 < len(toks); i++ {
		name, at := "", -1
		switch {
		case toks[i].kind == tokIdent && toks[i].text == "var" && toks[i+1].kind == tokIdent:
			// var orders []Order
			name, at = toks[i+1].text, i+2
		case toks[i].kind == tokIdent && i+2 < len(toks) &&
			isPunct(toks[i+1], ":") && isPunct(toks[i+2], "="):
			// orders := []Order{}
			name, at = toks[i].text, i+3
		case toks[i].kind == tokIdent && (isPunct(toks[i-1], "(") || isPunct(toks[i-1], ",")):
			// A parameter or a struct field: `func f(o *Order)`. The type is
			// stated at the declaration, which is the whole reason `repo.Save(o)`
			// can be attributed to a table at all.
			name, at = toks[i].text, i+1
		}
		if name == "" || at >= len(toks) || isTypeName(name) {
			continue
		}
		if ty := goDeclaredType(toks, at); ty != "" {
			out[name] = ty
		}
	}
	return out
}

// goDeclaredType reads a type expression, and returns the type it names.
//
// A type expression is pointers, slices, qualification and identifiers, and
// nothing else. The scan stops at the end of the line and at the first token
// that cannot appear in one, so `x := repo.Load()` yields nothing: `(` means a
// call, and a call states no type. That refusal is the point — treating `Load`
// as an entity would put a table called `loads` in the graph.
func goDeclaredType(toks []token, i int) string {
	limit := i + 10
	if limit > len(toks) {
		limit = len(toks)
	}
	line := toks[i].line
	last := ""
	makeOrNew := false
	for j := i; j < limit && toks[j].line == line; j++ {
		t := toks[j]
		switch {
		case isPunct(t, "&"), isPunct(t, "*"), isPunct(t, "["), isPunct(t, "]"), isPunct(t, "."):
			continue
		case t.kind == tokIdent && last == "" && (t.text == "make" || t.text == "new"):
			makeOrNew = true
			continue
		case isPunct(t, "(") && makeOrNew:
			continue
		case t.kind == tokIdent:
			last = t.text
			continue
		case isPunct(t, "("):
			// `Load(` — a call, not a type.
			return ""
		}
		break
	}
	if isTypeName(last) {
		return last
	}
	return ""
}
