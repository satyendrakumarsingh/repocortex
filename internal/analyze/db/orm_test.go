package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The ORM extractors are held to the same two standards as the SQL recogniser:
// be right about what is claimed, and claim nothing about a file that is not
// using the framework. So every ecosystem gets a fixture asserting the table,
// the access kind and the line the access is attributed from — and a negative
// fixture using the same vocabulary with none of the framework, which must
// produce nothing at all.

type wantAccess struct {
	line    int
	verb    string
	access  Access
	tables  []string
	columns []string
	certain bool
	// symbol is the definition the access must be attributed to, under the
	// same innermost-line-containment rule internal/analyze applies.
	symbol string
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("testdata", "orm", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return src
}

func checkFixture(t *testing.T, name string, want []wantAccess) {
	t.Helper()
	src := fixture(t, name)
	got := ORMStatements(name, src)

	if len(got) != len(want) {
		for _, g := range got {
			t.Logf("got: line=%d verb=%s access=%s tables=%v cols=%v certain=%v",
				g.Line, g.Verb, g.Access, g.Tables, g.Columns, g.Certain)
		}
		t.Fatalf("%s: got %d accesses, want %d", name, len(got), len(want))
	}
	for i, w := range want {
		g := got[i]
		if g.Line != w.line {
			t.Errorf("%s[%d]: line %d, want %d", name, i, g.Line, w.line)
		}
		if g.Verb != w.verb {
			t.Errorf("%s[%d]: verb %q, want %q", name, i, g.Verb, w.verb)
		}
		if g.Access != w.access {
			t.Errorf("%s[%d] line %d: access %s, want %s", name, i, g.Line, g.Access, w.access)
		}
		if !equalStrings(g.Tables, w.tables) {
			t.Errorf("%s[%d] line %d: tables %v, want %v", name, i, g.Line, g.Tables, w.tables)
		}
		if w.columns != nil && !equalStrings(g.Columns, w.columns) {
			t.Errorf("%s[%d] line %d: columns %v, want %v", name, i, g.Line, g.Columns, w.columns)
		}
		if g.Certain != w.certain {
			t.Errorf("%s[%d] line %d: certain %v, want %v", name, i, g.Line, g.Certain, w.certain)
		}
		if w.symbol != "" {
			if sym := enclosingSymbol(string(src), g.Line); sym != w.symbol {
				t.Errorf("%s[%d] line %d: attributed to %q, want %q", name, i, g.Line, sym, w.symbol)
			}
		}
	}
}

// TestGORMExtraction covers all four ways a GORM chain names a table, and the
// two access kinds a naive scan gets wrong: an Update behind a Model, and a
// Delete behind a Where.
func TestGORMExtraction(t *testing.T) {
	checkFixture(t, "store.go", []wantAccess{
		{
			// TableName() states the table, so the file DECLARES it.
			line: 18, verb: "gorm.TableName", access: AccessDDL,
			tables: []string{"orders"}, certain: true, symbol: "TableName",
		},
		{
			// The entity arrives as a parameter, whose declared type the file
			// states. File-scoped resolution, hence not certain.
			line: 32, verb: "gorm.Create", access: AccessWrite,
			tables: []string{"orders"}, certain: false, symbol: "CreateOrder",
		},
		{
			line: 37, verb: "gorm.Find", access: AccessRead,
			tables: []string{"orders"}, certain: false, symbol: "FindByMerchant",
		},
		{
			// Model names the entity and Update is the operation: a WRITE, not
			// the READ a `Where` might suggest.
			line: 42, verb: "gorm.Update", access: AccessWrite,
			tables: []string{"orders"}, certain: true, symbol: "MarkPaid",
		},
		{
			// OrderItem states no table, so GORM's default naming applies:
			// snake_case then pluralised.
			line: 46, verb: "gorm.Delete", access: AccessWrite,
			tables: []string{"order_items"}, certain: false, symbol: "PurgeItems",
		},
		{
			line: 50, verb: "gorm.AutoMigrate", access: AccessDDL,
			tables: []string{"orders", "order_items"}, certain: false, symbol: "Migrate",
		},
		{
			// A table this repository never declares. Recorded anyway: an
			// access that names a table repocortex does not own still states
			// something true, and migration 0003 exists to hold it.
			line: 55, verb: "gorm.Count", access: AccessRead,
			tables: []string{"legacy_orders"}, certain: true, symbol: "CountLegacy",
		},
	})
}

func TestJPAEntityDeclaresTableAndColumns(t *testing.T) {
	checkFixture(t, "Payment.java", []wantAccess{{
		line: 14, verb: "jpa.@Entity", access: AccessDDL,
		tables:  []string{"payment"},
		columns: []string{"payment_id", "merchant_id"},
		certain: true, symbol: "Payment",
	}})
}

// TestJPARepositoryAccesses is the case that makes JPA worth doing: a Spring
// Data interface contains no query text at all, and every fact about it is in
// its type parameter and its method names.
func TestJPARepositoryAccesses(t *testing.T) {
	checkFixture(t, "PaymentRepository.java", []wantAccess{
		{
			// Extending JpaRepository IS the declaration of CRUD on Payment.
			line: 8, verb: "jpa.JpaRepository", access: AccessReadWrite,
			tables: []string{"payment"}, certain: true, symbol: "PaymentRepository",
		},
		{
			line: 10, verb: "jpa.findByMerchantId", access: AccessRead,
			tables: []string{"payment"}, certain: true, symbol: "findByMerchantId",
		},
		{
			line: 12, verb: "jpa.countByStatus", access: AccessRead,
			tables: []string{"payment"}, certain: true, symbol: "countByStatus",
		},
		{
			// `deleteBy` is a write. Classified from the Spring Data subject
			// keyword, which is a declaration, not a guess about a name.
			line: 14, verb: "jpa.deleteByCreatedAtBefore", access: AccessWrite,
			tables: []string{"payment"}, certain: true, symbol: "deleteByCreatedAtBefore",
		},
	})
}

func TestJPAEntityManagerAccesses(t *testing.T) {
	checkFixture(t, "PaymentDao.java", []wantAccess{
		{
			line: 12, verb: "jpa.find", access: AccessRead,
			tables: []string{"payment"}, certain: true, symbol: "load",
		},
		{
			line: 16, verb: "jpa.persist", access: AccessWrite,
			tables: []string{"payment"}, certain: true, symbol: "store",
		},
	})
}

func TestSQLAlchemyExtraction(t *testing.T) {
	checkFixture(t, "models.py", []wantAccess{
		{
			line: 9, verb: "sqlalchemy.__tablename__", access: AccessDDL,
			tables: []string{"orders"},
			// `merchant`, not `merchant_id`: Column's first positional argument
			// is the database name when it is given, and the attribute name
			// only when it is not.
			columns: []string{"id", "merchant", "status"},
			certain: true, symbol: "Order",
		},
		{
			line: 17, verb: "sqlalchemy.__tablename__", access: AccessDDL,
			tables: []string{"shipments"}, columns: []string{"id"},
			certain: true, symbol: "Shipment",
		},
		{
			line: 24, verb: "sqlalchemy.query", access: AccessRead,
			tables: []string{"orders"}, certain: true, symbol: "list_orders",
		},
		{
			line: 29, verb: "sqlalchemy.add", access: AccessWrite,
			tables: []string{"orders"}, certain: true, symbol: "create_order",
		},
		{
			// A query chain that ends in delete() is a WRITE. Classifying on
			// `query` alone would report the file's only destructive statement
			// as a read.
			line: 34, verb: "sqlalchemy.query", access: AccessWrite,
			tables: []string{"shipments"}, certain: true, symbol: "purge_shipments",
		},
		{
			// The Core construct inside session.execute(...): the outer call
			// names no entity, the inner one does.
			line: 38, verb: "sqlalchemy.select", access: AccessRead,
			tables: []string{"orders"}, certain: true, symbol: "recent",
		},
	})
}

// TestNonORMFilesYieldNothing is the test that matters most.
//
// Each fixture uses the vocabulary the extractors look for — save, find,
// delete, query, add, Model, Table — with none of the framework. A single
// reported access here would mean repocortex had invented a table, which this
// package's own doc comment calls the failure that costs the reader's trust in
// every other row.
func TestNonORMFilesYieldNothing(t *testing.T) {
	for _, name := range []string{"plain.go", "PlainService.java", "plain.py"} {
		src := fixture(t, name)
		if got := ORMStatements(name, src); len(got) != 0 {
			for _, g := range got {
				t.Errorf("%s: invented %v (%s) at line %d", name, g.Tables, g.Access, g.Line)
			}
		}
	}
}

// TestUnknownLanguagesAreNotGuessedAt: the three ecosystems chosen are the
// three implemented. A Ruby or TypeScript file must produce nothing rather than
// a partial answer, because a partial answer is indistinguishable from a
// complete one downstream.
func TestUnknownLanguagesAreNotGuessedAt(t *testing.T) {
	src := []byte(`class Order < ActiveRecord::Base; end
Order.where(status: "paid").destroy_all`)
	for _, path := range []string{"order.rb", "order.ts", "order.php", "Order.kt", "order"} {
		if got := ORMStatements(path, src); len(got) != 0 {
			t.Errorf("%s: got %d accesses, want none — that ecosystem is not implemented", path, len(got))
		}
	}
}

// TestDefaultNamingConventions pins the three conventions the extractors fall
// back to, because a convention stated in a doc comment and not in a test is a
// convention that drifts.
func TestDefaultNamingConventions(t *testing.T) {
	t.Run("gorm snake_case then pluralise", func(t *testing.T) {
		cases := map[string]string{
			"User":         "users",
			"Order":        "orders",
			"OrderItem":    "order_items",
			"Address":      "addresses",
			"Category":     "categories",
			"Person":       "people",
			"HTTPEndpoint": "http_endpoints",
		}
		for in, want := range cases {
			if got := gormDefaultTable(in); got != want {
				t.Errorf("gormDefaultTable(%q) = %q, want %q", in, got, want)
			}
		}
	})

	t.Run("jpa camel to underscores, certain only when both strategies agree", func(t *testing.T) {
		cases := []struct {
			in      string
			table   string
			certain bool
		}{
			// Spring Boot's CamelCaseToUnderscoresNamingStrategy and plain
			// Hibernate's standard strategy produce the same lowercased name
			// for a single word, and differ for a compound one.
			{"Payment", "payment", true},
			{"Customer", "customer", true},
			{"OrderItem", "order_item", false},
			{"MerchantAccount", "merchant_account", false},
		}
		for _, c := range cases {
			got := jpaDefaultTable(c.in)
			if got.table != c.table || got.certain != c.certain {
				t.Errorf("jpaDefaultTable(%q) = %q/%v, want %q/%v",
					c.in, got.table, got.certain, c.table, c.certain)
			}
		}
	})

	t.Run("sqlalchemy declarative has no default", func(t *testing.T) {
		// A declarative class without __tablename__ raises InvalidRequestError,
		// so there is no convention to fall back to and nothing may be
		// reported. Only Flask-SQLAlchemy's db.Model supplies one.
		src := []byte(`from sqlalchemy.orm import declarative_base
Base = declarative_base()

class Order(Base):
    pass

def go(session):
    return session.query(Order).all()
`)
		if got := ORMStatements("m.py", src); len(got) != 0 {
			t.Fatalf("got %d accesses, want none: SQLAlchemy declares no default table name", len(got))
		}

		flask := []byte(`from flask_sqlalchemy import SQLAlchemy

db = SQLAlchemy()

class OrderItem(db.Model):
    id = db.Column(db.Integer, primary_key=True)

def go(session):
    return session.query(OrderItem).all()
`)
		got := ORMStatements("m.py", flask)
		if len(got) != 1 {
			t.Fatalf("flask: got %d accesses, want 1", len(got))
		}
		// Flask-SQLAlchemy's documented default: camel-case to snake_case,
		// with no pluralisation. Not certain, and so not allowed to declare a
		// table node — only to name one in an access.
		if got[0].Tables[0] != "order_item" || got[0].Access != AccessRead || got[0].Certain {
			t.Errorf("flask: got %v %s certain=%v, want [order_item] READ certain=false",
				got[0].Tables, got[0].Access, got[0].Certain)
		}
	})
}

// TestEmptyAndOversizedInput: the bounds, which exist so one generated file
// cannot turn indexing into an allocation storm.
func TestEmptyAndOversizedInput(t *testing.T) {
	if got := ORMStatements("x.go", nil); got != nil {
		t.Errorf("nil source produced %v", got)
	}
	big := make([]byte, maxORMBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	if got := ORMStatements("x.go", big); got != nil {
		t.Errorf("oversized source produced %v", got)
	}
}

// TestEveryStatementIsStageable holds the contract the caller depends on: an
// ORM statement is staged through exactly the path raw SQL uses, so it must
// carry everything that path reads.
func TestEveryStatementIsStageable(t *testing.T) {
	for _, name := range []string{"store.go", "Payment.java", "PaymentRepository.java",
		"PaymentDao.java", "models.py"} {
		for _, st := range ORMStatements(name, fixture(t, name)) {
			if len(st.Tables) == 0 {
				t.Errorf("%s line %d: no table — an access with no name states nothing", name, st.Line)
			}
			if st.Access == "" {
				t.Errorf("%s line %d: no access mode", name, st.Line)
			}
			if st.Verb == "" {
				t.Errorf("%s line %d: no verb", name, st.Line)
			}
			if st.Line <= 0 {
				t.Errorf("%s: line %d cannot be attributed to a symbol", name, st.Line)
			}
			for _, tbl := range st.Tables {
				if tbl != strings.ToLower(tbl) || strings.Contains(tbl, ".") {
					t.Errorf("%s line %d: table %q is not lowercased and unqualified", name, st.Line, tbl)
				}
			}
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// enclosingSymbol mirrors internal/analyze's attribution rule — the innermost
// definition whose range covers the line — closely enough to assert against.
//
// It is deliberately a separate implementation rather than a call into
// analyze: the extractors' contribution to attribution is the LINE, and this
// checks that the line lands inside the definition a reader would name.
func enclosingSymbol(src string, line int) string {
	lines := strings.Split(src, "\n")
	if line < 1 || line > len(lines) {
		return ""
	}
	for i := line - 1; i >= 0; i-- {
		text := strings.TrimSpace(lines[i])
		switch {
		case strings.HasPrefix(text, "func "):
			return goFuncName(text)
		case strings.HasPrefix(text, "def "):
			return identAfter(text, "def ")
		case strings.HasPrefix(text, "class "):
			return identAfter(text, "class ")
		case strings.Contains(text, " class "):
			return identAfter(text, " class ")
		case strings.Contains(text, " interface "):
			return identAfter(text, " interface ")
		}
		// A Java member declaration: an identifier immediately before a
		// parenthesis, on a line that is not a call.
		if j := strings.Index(text, "("); j > 0 && (strings.HasSuffix(text, "{") || strings.HasSuffix(text, ";")) &&
			!strings.Contains(text[:j], ".") && !strings.Contains(text[:j], "=") &&
			strings.Contains(text[:j], " ") {
			return lastIdent(text[:j])
		}
	}
	return ""
}

func goFuncName(text string) string {
	rest := strings.TrimPrefix(text, "func ")
	if strings.HasPrefix(rest, "(") {
		if k := strings.Index(rest, ")"); k >= 0 {
			rest = strings.TrimSpace(rest[k+1:])
		}
	}
	return leadingIdent(rest)
}

func identAfter(text, keyword string) string {
	k := strings.Index(text, keyword)
	if k < 0 {
		return ""
	}
	return leadingIdent(strings.TrimSpace(text[k+len(keyword):]))
}

func leadingIdent(s string) string {
	for i := 0; i < len(s); i++ {
		if !isIdentPart(s[i]) {
			return s[:i]
		}
	}
	return s
}

func lastIdent(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

func TestPrismaExtraction(t *testing.T) {
	t.Run("schema declaration", func(t *testing.T) {
		schema := `datasource db { provider = "postgresql" url = env("DATABASE_URL") }
generator client { provider = "prisma-client-js" }

model User {
  id    Int     @id @default(autoincrement())
  email String  @unique
  name  String?
  posts Post[]
  @@map("users")
}

model OrderItem {
  id       Int @id
  quantity Int
}
`
		stmts := ORMStatements("schema.prisma", []byte(schema))
		if len(stmts) != 2 {
			t.Fatalf("got %d statements, want 2", len(stmts))
		}

		user := stmts[0]
		if user.Verb != "prisma.model" || user.Access != AccessDDL {
			t.Errorf("user: verb=%s access=%s", user.Verb, user.Access)
		}
		if len(user.Tables) != 1 || user.Tables[0] != "users" {
			t.Errorf("user tables = %v, want [users]", user.Tables)
		}
		if !user.Certain {
			t.Errorf("user should be certain due to @@map")
		}

		orderItem := stmts[1]
		if orderItem.Verb != "prisma.model" || orderItem.Access != AccessDDL {
			t.Errorf("orderItem: verb=%s access=%s", orderItem.Verb, orderItem.Access)
		}
		if len(orderItem.Tables) != 1 || orderItem.Tables[0] != "order_item" {
			t.Errorf("orderItem tables = %v, want [order_item]", orderItem.Tables)
		}
	})

	t.Run("typescript client usage", func(t *testing.T) {
		tsCode := `import { PrismaClient } from '@prisma/client';
const prisma = new PrismaClient();

export async function getUser(id: number) {
  return await prisma.user.findUnique({ where: { id } });
}

export async function createOrder(data: any) {
  return await prisma.order.create({ data });
}

export class PaymentService {
  constructor(private prisma: PrismaClient) {}
  async cancel(id: string) {
    await this.prisma.payment.delete({ where: { id } });
  }
}
`
		stmts := ORMStatements("service.ts", []byte(tsCode))
		if len(stmts) != 3 {
			t.Fatalf("got %d statements, want 3", len(stmts))
		}

		// 1. findUnique on user
		if stmts[0].Verb != "prisma.findUnique" || stmts[0].Access != AccessRead {
			t.Errorf("stmt[0]: verb=%s access=%s", stmts[0].Verb, stmts[0].Access)
		}
		if len(stmts[0].Tables) != 1 || stmts[0].Tables[0] != "user" {
			t.Errorf("stmt[0] table = %v, want [user]", stmts[0].Tables)
		}

		// 2. create on order
		if stmts[1].Verb != "prisma.create" || stmts[1].Access != AccessWrite {
			t.Errorf("stmt[1]: verb=%s access=%s", stmts[1].Verb, stmts[1].Access)
		}
		if len(stmts[1].Tables) != 1 || stmts[1].Tables[0] != "order" {
			t.Errorf("stmt[1] table = %v, want [order]", stmts[1].Tables)
		}

		// 3. delete on payment
		if stmts[2].Verb != "prisma.delete" || stmts[2].Access != AccessWrite {
			t.Errorf("stmt[2]: verb=%s access=%s", stmts[2].Verb, stmts[2].Access)
		}
		if len(stmts[2].Tables) != 1 || stmts[2].Tables[0] != "payment" {
			t.Errorf("stmt[2] table = %v, want [payment]", stmts[2].Tables)
		}
	})

	t.Run("negative without prisma gate", func(t *testing.T) {
		code := `const repo = getRepo();
const u = repo.user.findMany();
const p = service.payment.create();
`
		stmts := ORMStatements("other.ts", []byte(code))
		if len(stmts) != 0 {
			t.Fatalf("expected 0 statements for non-prisma file, got %d", len(stmts))
		}
	})
}
