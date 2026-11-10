package db

import "strings"

// Prisma ORM (prisma.io).
//
// # Two declarations: Schema and Client
//
// Prisma splits persistence across two representations:
//   - `schema.prisma`: `model User { id Int @id @@map("users") }` declares
//     the schema, table mappings, and columns.
//   - TypeScript / JavaScript client: `prisma.user.findMany()` or
//     `prisma.order.create()` performs typed queries and mutations against models.
//
// # The gate
//
// For TypeScript / JavaScript, the file must reference `@prisma/client`,
// `PrismaClient`, or a known Prisma handle (`prisma`, `prismaClient`).
// Plain calls to methods named `findMany` or `create` on arbitrary objects
// are ignored to avoid false positives.

var prismaRead = map[string]bool{
	"findUnique":        true,
	"findUniqueOrThrow": true,
	"findFirst":         true,
	"findFirstOrThrow":  true,
	"findMany":          true,
	"count":             true,
	"aggregate":         true,
	"groupBy":           true,
	"$queryRaw":         true,
	"$queryRawUnsafe":   true,
}

var prismaWrite = map[string]bool{
	"create":              true,
	"createMany":          true,
	"createManyAndReturn": true,
	"update":              true,
	"updateMany":          true,
	"delete":              true,
	"deleteMany":          true,
	"upsert":              true,
	"$executeRaw":         true,
	"$executeRawUnsafe":   true,
}

// prismaSchemaStatements extracts table declarations and column mappings from schema.prisma.
func prismaSchemaStatements(src string) []ORMStatement {
	toks := lexBraced(src)
	var out []ORMStatement

	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.kind != tokIdent || t.text != "model" {
			continue
		}
		// Expect: model <Name> { ... }
		if i+2 >= len(toks) || toks[i+1].kind != tokIdent || toks[i+2].text != "{" {
			continue
		}
		modelName := toks[i+1].text
		modelLine := t.line

		// Scan inside block until matching }
		depth := 0
		tableName := snakeCase(modelName)
		var cols []string
		mapped := false

		j := i + 2
		for ; j < len(toks); j++ {
			tok := toks[j]
			if tok.text == "{" {
				depth++
				continue
			}
			if tok.text == "}" {
				depth--
				if depth == 0 {
					break
				}
				continue
			}
			if depth == 1 {
				// Check for @@map("custom_table_name")
				if tok.text == "@" && j+1 < len(toks) && toks[j+1].text == "@" {
					if j+2 < len(toks) && toks[j+2].text == "map" {
						if j+4 < len(toks) && toks[j+3].text == "(" && toks[j+4].kind == tokString {
							tableName = strings.ToLower(toks[j+4].text)
							mapped = true
						}
					}
				}
				// Check for field declarations: identifier at line start
				if tok.kind == tokIdent && !strings.HasPrefix(tok.text, "@") {
					// Ensure it's not a block-level keyword
					if tok.text != "model" && tok.text != "enum" && tok.text != "datasource" && tok.text != "generator" {
						// Only collect field if followed by a type identifier
						if j+1 < len(toks) && toks[j+1].kind == tokIdent {
							cols = append(cols, tok.text)
						}
					}
				}
			}
		}
		i = j

		stmt := ORMStatement{
			Statement: Statement{
				Verb:    "prisma.model",
				Access:  AccessDDL,
				Tables:  []string{tableName},
				Columns: cols,
				Certain: mapped || !isCompound(modelName),
			},
			Line:      modelLine,
			Framework: "prisma",
		}
		out = append(out, stmt)
	}
	return out
}

// prismaClientStatements extracts Prisma Client operations in TypeScript/JavaScript.
func prismaClientStatements(src string) []ORMStatement {
	toks := lexBraced(src)
	if !usesPrisma(toks) {
		return nil
	}

	var out []ORMStatement
	for i := 0; i < len(toks); i++ {
		// Look for pattern: <receiver> . <model> . <op> (
		// Examples:
		//   prisma.user.findMany(
		//   this.prisma.order.create(
		//   ctx.prisma.payment.delete(
		//   prismaClient.account.update(
		if toks[i].kind != tokIdent {
			continue
		}
		name := toks[i].text
		if name != "prisma" && name != "prismaClient" && name != "db" {
			continue
		}

		if i+4 >= len(toks) {
			continue
		}
		if toks[i+1].text != "." || toks[i+2].kind != tokIdent || toks[i+3].text != "." || toks[i+4].kind != tokIdent {
			continue
		}

		modelName := toks[i+2].text
		op := toks[i+4].text
		opLine := toks[i+4].line

		var access Access
		switch {
		case prismaRead[op]:
			access = AccessRead
		case prismaWrite[op]:
			access = AccessWrite
		default:
			continue
		}

		table := snakeCase(modelName)
		out = append(out, ORMStatement{
			Statement: Statement{
				Verb:    "prisma." + op,
				Access:  access,
				Tables:  []string{table},
				Certain: false,
			},
			Line:      opLine,
			Framework: "prisma",
		})
	}

	return out
}

// usesPrisma checks whether the token stream contains evidence of Prisma Client usage.
func usesPrisma(toks []token) bool {
	for i, t := range toks {
		if t.kind == tokString && (strings.Contains(t.text, "@prisma/client") || strings.Contains(t.text, ".prisma/client")) {
			return true
		}
		if t.kind == tokIdent {
			if t.text == "PrismaClient" {
				return true
			}
			if (t.text == "prisma" || t.text == "prismaClient") && i+1 < len(toks) && toks[i+1].text == "." {
				return true
			}
		}
	}
	return false
}
