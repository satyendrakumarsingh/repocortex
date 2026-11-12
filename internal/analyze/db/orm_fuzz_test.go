package db

import (
	"strings"
	"testing"
)

// The ORM extractors run over source RepoCortex did not write, so "must not
// panic" is a security property rather than a tidiness one — a single malformed
// file in a dependency's vendor directory must not stop `repocortex index`.
//
// Four invariants are checked on every input, and each corresponds to something
// a caller downstream relies on:
//
//   - No panic. The scanners index into byte slices and token slices with
//     computed offsets, which is exactly where truncated input bites.
//   - Every reported statement names at least one table, and every table is a
//     usable name. A row in table_accesses with an empty or whitespace-bearing
//     table_name is a row nothing can ever bind.
//   - Every statement carries an access mode and a positive line, because the
//     staging path reads both unconditionally.
//   - The result is deterministic. Entity maps are built with Go maps, and a
//     single leaked iteration order would make the index differ between runs of
//     the same commit — which looks exactly like a real change to every
//     incremental-reindex test in the repository.

func fuzzORMInvariants(t *testing.T, path, s string) {
	t.Helper()
	got := ORMStatements(path, []byte(s))
	for _, st := range got {
		if len(st.Tables) == 0 {
			t.Fatalf("%s: statement with no table from %q", path, s)
		}
		if st.Access == "" {
			t.Fatalf("%s: statement with no access mode from %q", path, s)
		}
		if st.Verb == "" {
			t.Fatalf("%s: statement with no verb from %q", path, s)
		}
		if st.Line <= 0 {
			t.Fatalf("%s: statement at line %d from %q", path, st.Line, s)
		}
		for _, tbl := range st.Tables {
			if tbl == "" {
				t.Fatalf("%s: empty table name from %q", path, s)
			}
			if strings.TrimSpace(tbl) != tbl || strings.ContainsAny(tbl, " \t\n\"`[].") {
				t.Fatalf("%s: unusable table name %q from %q", path, tbl, s)
			}
			if tbl != strings.ToLower(tbl) {
				t.Fatalf("%s: table %q is not lowercased, from %q", path, tbl, s)
			}
		}
		for _, col := range st.Columns {
			if col == "" {
				t.Fatalf("%s: empty column name from %q", path, s)
			}
		}
	}

	again := ORMStatements(path, []byte(s))
	if len(again) != len(got) {
		t.Fatalf("%s: nondeterministic result for %q", path, s)
	}
	for i := range got {
		if got[i].Line != again[i].Line || got[i].Verb != again[i].Verb ||
			got[i].Access != again[i].Access ||
			strings.Join(got[i].Tables, ",") != strings.Join(again[i].Tables, ",") {
			t.Fatalf("%s: nondeterministic result for %q", path, s)
		}
	}
}

func FuzzGORMStatements(f *testing.F) {
	for _, s := range []string{
		"package p\nimport \"gorm.io/gorm\"\nfunc f(db *gorm.DB) { db.Model(&User{}).Update(\"a\", 1) }",
		"package p\nimport \"gorm.io/gorm\"\nfunc f(db *gorm.DB) { db.Table(\"t\").Count(&n) }",
		"package p\nimport \"gorm.io/gorm\"\nfunc (U) TableName() string { return \"u\" }",
		"package p\nimport \"gorm.io/gorm\"\nvar x = db.AutoMigrate(&A{}, &B{})",
		"import \"gorm.io/gorm\"\n`",
		"gorm:\"",
		"package p\nimport \"gorm.io/gorm\"\nfunc (",
		"",
		"((((",
		"/*",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) { fuzzORMInvariants(t, "x.go", s) })
}

func FuzzJPAStatements(f *testing.F) {
	for _, s := range []string{
		"import jakarta.persistence.Entity;\n@Entity\n@Table(name = \"orders\")\nclass Order {}",
		"@Entity class Payment { @Column(name = \"pid\") Long id; }",
		"interface R extends JpaRepository<Order, Long> { List<Order> findByX(String x); }",
		"import jakarta.persistence.EntityManager;\nclass D { EntityManager em; void f(){ em.persist(o); } }",
		"@Entity class",
		"@Table(name =",
		"interface R extends JpaRepository<",
		"\"\"\"",
		"",
		"}}}}",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) { fuzzORMInvariants(t, "X.java", s) })
}

func FuzzSQLAlchemyStatements(f *testing.F) {
	for _, s := range []string{
		"from sqlalchemy import Column\nclass O(Base):\n    __tablename__ = \"o\"\n    id = Column(Integer)",
		"from sqlalchemy import select\ndef f(session):\n    return session.query(O).delete()",
		"from sqlalchemy import Table\nt = Table(\"t\", meta, Column(\"c\"))",
		"from flask_sqlalchemy import SQLAlchemy\nclass OrderItem(db.Model):\n    pass",
		"from sqlalchemy import\nclass",
		"from sqlalchemy import x\n'''",
		"from sqlalchemy import x\n__tablename__ = \"\"",
		"",
		"(((",
		"#",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) { fuzzORMInvariants(t, "m.py", s) })
}
