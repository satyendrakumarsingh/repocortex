package infra

import (
	"fmt"
	"regexp"
	"strings"
)

// Terraform is read with a bounded block scanner rather than an HCL parser, and
// that is a decision with a reason rather than a shortcut.
//
// Nothing in go.mod parses HCL. Adding hashicorp/hcl would pull a large
// dependency tree into a binary whose redaction path is deliberately
// standard-library-only (see internal/security's package comment), for the sake
// of one thing: the labels on a block header. So this reads block HEADERS —
// `resource "aws_db_instance" "main"` — and stops.
//
// The bound has a useful consequence. A .tf file routinely contains
// `password = "…"`, `default = "…"` on a secret variable, and an inline
// provider credential. This scanner never reads an attribute at all, so there is
// no path from an attribute value into the output. What it gives up is the
// attribute-level detail — which port an `aws_security_group_rule` opens, which
// image an `aws_ecs_task_definition` runs — and that limit is reported as part
// of the format rather than left for a reader to discover.
//
// What it must still get right is which braces are real: a `#` inside a string
// is not a comment, and a heredoc body can contain anything at all including
// unbalanced braces. Both are handled below, because getting either wrong makes
// the depth count drift and turns nested blocks into top-level resources.

var tfBlockHeader = regexp.MustCompile(
	`^\s*(resource|data|module|provider|variable|output|locals|terraform)\b\s*` +
		`(?:"([^"]*)")?\s*(?:"([^"]*)")?`)

// maxTerraformLines bounds one file.
const maxTerraformLines = 100000

func parseTerraform(path string, src []byte) File {
	f := File{Path: path, Format: FormatTerraform}

	var (
		depth      int
		inComment  bool // inside /* … */
		heredoc    string
		vars       KeySet
		lines      = strings.Split(string(src), "\n")
		truncated  bool
		blockLimit = maxUnits
	)
	vars.Type = "terraform-variables"
	vars.Name = fileBase(path)

	for i, raw := range lines {
		if i >= maxTerraformLines {
			truncated = true
			break
		}

		if heredoc != "" {
			// Heredoc bodies are skipped whole. They are the one place in a .tf
			// file where arbitrary text — a script, a policy document, a
			// certificate — sits at what would otherwise look like statement
			// level.
			if strings.TrimSpace(raw) == heredoc {
				heredoc = ""
			}
			continue
		}

		code, delta, nextHeredoc, nextComment := tfScanLine(raw, inComment)
		inComment = nextComment

		if depth == 0 {
			if m := tfBlockHeader.FindStringSubmatch(code); m != nil {
				if len(f.Units)+len(vars.Keys) >= blockLimit {
					truncated = true
					break
				}
				addTerraformBlock(&f, &vars, m, i+1)
			}
		}

		depth += delta
		if depth < 0 {
			// A closing brace with nothing open means the scanner's idea of
			// nesting has drifted from the file's — a malformed file, or a
			// construct this reader does not understand. Resetting to zero and
			// carrying on keeps the remaining blocks readable, and the reset is
			// recorded so the file is never reported as fully read.
			depth = 0
			f.Problems = append(f.Problems, Problem{
				Reason: ReasonMalformedYAML,
				Line:   i + 1,
				Detail: "unbalanced braces; block nesting could not be tracked from here",
			})
		}
		heredoc = nextHeredoc
	}

	if truncated {
		f.Problems = append(f.Problems, Problem{
			Reason: ReasonLimit,
			Detail: fmt.Sprintf("stopped after %d blocks or %d lines", blockLimit, maxTerraformLines),
		})
	}
	if len(vars.Keys) > 0 {
		f.KeySets = append(f.KeySets, vars)
	}
	return f
}

// addTerraformBlock turns one matched header into what it declares.
//
// Variables are collected as a key set rather than as units: a variable is not a
// thing that runs, it is the configuration surface of the module — the same role
// a Helm values file plays, and it belongs next to it in a report.
func addTerraformBlock(f *File, vars *KeySet, m []string, line int) {
	kind, first, second := m[1], m[2], m[3]

	switch kind {
	case "variable":
		if k, ok := newEnvKey(first, line, SourceLiteral, ""); ok {
			if vars.Line == 0 {
				vars.Line = line
			}
			vars.Keys = append(vars.Keys, k)
		}
	case "resource", "data":
		typ, ok := cleanName(first)
		if !ok {
			return
		}
		name, ok := cleanName(second)
		if !ok {
			// A resource with no second label is not valid Terraform, but the
			// type is still a declaration of intent worth listing.
			name = "(unnamed)"
		}
		f.Units = append(f.Units, Unit{Name: name, Type: kind + ":" + typ, Line: line})
	case "module", "provider":
		name, ok := cleanName(first)
		if !ok {
			return
		}
		f.Units = append(f.Units, Unit{Name: name, Type: kind, Line: line})
	case "output", "locals", "terraform":
		// Deliberately not reported. An output is a value this module publishes
		// and `locals`/`terraform` are bookkeeping; none of the three is a
		// service, a dependency or a setting, and listing them would pad the
		// inventory with rows nobody acts on.
	}
}

// tfScanLine strips comments, counts braces outside strings, and detects the
// start of a heredoc.
//
// Returns the line with comments removed but STRING CONTENTS INTACT, because the
// block header's labels are strings and the header regex needs them.
func tfScanLine(line string, inComment bool) (code string, delta int, heredoc string, stillComment bool) {
	var b strings.Builder
	inStr := false

	for i := 0; i < len(line); i++ {
		c := line[i]

		if inComment {
			if c == '*' && i+1 < len(line) && line[i+1] == '/' {
				inComment = false
				i++
			}
			continue
		}

		if inStr {
			b.WriteByte(c)
			switch {
			case c == '\\' && i+1 < len(line):
				i++
				b.WriteByte(line[i])
			case c == '"':
				inStr = false
			}
			continue
		}

		switch {
		case c == '"':
			inStr = true
			b.WriteByte(c)
		case c == '#':
			return b.String(), delta, "", false
		case c == '/' && i+1 < len(line) && line[i+1] == '/':
			return b.String(), delta, "", false
		case c == '/' && i+1 < len(line) && line[i+1] == '*':
			inComment = true
			i++
		case c == '<' && i+1 < len(line) && line[i+1] == '<':
			// `<<TAG` or `<<-TAG`. Everything from here to the terminator is
			// opaque text, so the rest of this line contributes no braces.
			if tag := heredocTag(line[i+2:]); tag != "" {
				return b.String(), delta, tag, inComment
			}
			b.WriteByte(c)
		case c == '{':
			delta++
			b.WriteByte(c)
		case c == '}':
			delta--
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), delta, "", inComment
}

// heredocTag reads the identifier that opens a heredoc, or "" when what follows
// `<<` is not one (`a << b` is a shift in an expression, not a heredoc).
func heredocTag(rest string) string {
	rest = strings.TrimPrefix(rest, "-")
	end := 0
	for end < len(rest) {
		c := rest[end]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' {
			end++
			continue
		}
		break
	}
	if end == 0 || end > 64 {
		return ""
	}
	// Anything after the tag on the same line means this was not a heredoc
	// opener; a real one ends the line.
	if strings.TrimSpace(rest[end:]) != "" {
		return ""
	}
	return rest[:end]
}

func fileBase(path string) string {
	base := strings.ReplaceAll(path, "\\", "/")
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if name, ok := cleanName(base); ok {
		return name
	}
	return "variables"
}
