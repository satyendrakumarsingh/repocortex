package parse

import (
	"path"
	"regexp"
	"strings"

	ts "github.com/odvcencio/gotreesitter"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// PowerShell. What the index holds, see fixtures/langs/powershell/expected.tsv:
//
//   - function, filter and workflow are a function, at any depth, under the
//     name written after the keyword with its scope modifier removed:
//     `function script:Get-Item` is Get-Item, as it is called;
//   - a DSC `Configuration Name { ... }` is a function;
//   - class and enum are a class;
//   - a method or constructor of a class is a method.
//
// Class properties, enum members, parameters and variables are not
// definitions. A class's base class (`class Dog : Animal`) is a use of Animal,
// not a second definition of it, so only the first name of the class counts.
func init() {
	registerDefinitions("powershell", powershellDefinitions, true)
	registerNameFix("powershell", powershellName)
	registerImportFix("powershell", powershellImport)
	definitionRefiners["powershell"] = powershellOwners
}

// powershellOwners says which class a method belongs to, so that its qualified
// name is `path:Account.Save` and a call on an Account finds it among the
// class's members and not among every `Save` of the repository. The grammar
// gives a class_statement no `name` field: its first simple_name is the class.
func powershellOwners(root *ts.Node, lang *ts.Language, _ string, src []byte, tags []ts.Tag) map[uint32]definitionFix {
	var fixes map[uint32]definitionFix
	for _, tag := range tags {
		if !strings.HasPrefix(tag.Kind, "definition.") || kindFromTag(tag.Kind) != model.KindMethod {
			continue
		}
		for n := root.NamedDescendantForByteRange(tag.NameRange.StartByte, tag.NameRange.EndByte); n != nil; n = n.Parent() {
			if n.Type(lang) != "class_statement" {
				continue
			}
			for i := 0; i < n.ChildCount(); i++ {
				if c := n.Child(i); c != nil && c.Type(lang) == "simple_name" {
					if fixes == nil {
						fixes = map[uint32]definitionFix{}
					}
					fixes[tag.NameRange.StartByte] = definitionFix{owner: c.Text(src)}
					break
				}
			}
			break
		}
	}
	return fixes
}

// powershellScope is the scope modifier that may precede a function's name.
var powershellScope = regexp.MustCompile(`(?i)^(global|script|local|private):`)

// powershellName drops the scope modifier from a function name. The same
// function is called without it, so the name with it matches no call.
func powershellName(name string) string {
	return powershellScope.ReplaceAllString(name, "")
}

const powershellDefinitions = `
; Calls. A command: a function or a cmdlet by name (Get-Thing -Arg 1), the head
; of a pipeline element or of a statement. Which names are the shell's own
; (Write-Host, Get-ChildItem) is the resolver's to say. "& Name" is the same call
; with the call operator in front, and a method call is $obj.M() or [Type]::M(),
; whose receiver shape.go reads from the text before the name.
(command (command_name) @name) @reference.call
(command (command_invokation_operator) @_op . (command_name_expr (command_name) @name @reference.call)
  (#eq? @_op "&"))
(invokation_expression (member_name (simple_name) @name)) @reference.call

(function_statement (function_name) @name) @definition.function
(class_statement . (simple_name) @name) @definition.class
(class_method_definition (simple_name) @name) @definition.method
(enum_statement . (simple_name) @name) @definition.class

; A DSC configuration is declared like a function: Configuration Name { ... }.
; The grammar reads it as a command whose first argument is the name and whose
; second is a script block.
((command (command_name) @_kw
   (command_elements . (command_argument_sep) . (generic_token) @name . (command_argument_sep)
     . (array_literal_expression (unary_expression (script_block_expression))))) @definition.function
  (#match? @_kw "^[Cc][Oo][Nn][Ff][Ii][Gg][Uu][Rr][Aa][Tt][Ii][Oo][Nn]$"))
`

var (
	// A path that starts at the script's own directory, in any of the spellings
	// a script uses for it.
	psScriptRoot = regexp.MustCompile(`(?i)^(?:\$\{?PSScriptRoot\}?|\$\(\s*Split-Path\s+(?:-Parent\s+)?\$(?:MyInvocation\.MyCommand\.(?:Path|Definition)|PSCommandPath)\s*\))[/\\]?`)
	// (Join-Path $PSScriptRoot 'lib' 'x.ps1'), and the older Split-Path form.
	psJoinPath  = regexp.MustCompile(`(?is)^\(\s*Join-Path\s+(\$\{?PSScriptRoot\}?|\(\s*Split-Path\s+(?:-Parent\s+)?\$[A-Za-z.]+\s*\))\s+(.+?)\s*\)$`)
	psStringArg = regexp.MustCompile(`^\s*(?:"([^"$]*)"|'([^']*)')`)
	// . $PSCommandPath.Replace('.Tests.ps1', '.ps1'): Pester's idiom for the file under test.
	psUnderTest = regexp.MustCompile(`(?i)^\$PSCommandPath\.Replace\(\s*['"]\.Tests\.ps1['"]\s*,\s*['"]\.ps1['"]\s*\)$`)
)

// powershellImport turns a dot-source, an Import-Module or a `using module`
// into what the resolver compares with a file.
//
//	. ./lib/util.ps1                    the file, relative to the script and as
//	                                    written (to the working directory)
//	. "$PSScriptRoot\lib\log.ps1"       relative to the script only; so is
//	. (Join-Path $PSScriptRoot 'x.ps1')   this form
//	Import-Module ./M.psm1 -Force       a module file; likewise `using module ./M.psm1`
//	Import-Module Billing               a module by name: a file or directory of that name
//	. $PSCommandPath.Replace('.Tests.ps1', '.ps1')
//	                                    a Pester test's file under test
//
// A path built from any other variable, an absolute path, `using namespace` and
// `& script.ps1` (which runs a script in a scope of its own, defining nothing
// here) name nothing. Backslashes are the separator on Windows and are read as
// slashes; the extension is dropped because the resolver compares stems.
func powershellImport(filePath string, imp Import) []Import {
	text := strings.TrimSpace(imp.Path)
	var arg string
	switch {
	case strings.HasPrefix(text, "."):
		arg = strings.TrimSpace(text[1:])
		if psUnderTest.MatchString(arg) {
			rooted := imp
			rooted.Path = strings.TrimSuffix(strings.TrimSuffix(filePath, path.Ext(filePath)), ".Tests")
			return []Import{rooted}
		}
	default:
		fields := psTokens(text)
		if len(fields) < 2 {
			return nil
		}
		args := fields[1:]
		if strings.EqualFold(fields[0], "using") {
			if !strings.EqualFold(args[0], "module") || len(args) < 2 {
				return nil
			}
			args = args[1:]
		}
		for i := 0; i < len(args); i++ {
			a := args[i]
			if strings.HasPrefix(a, "-") {
				if strings.EqualFold(a, "-Name") || strings.EqualFold(a, "-Path") {
					if i+1 < len(args) {
						arg = args[i+1]
					}
					break
				}
				continue
			}
			arg = a
			break
		}
		if arg == "" {
			return nil
		}
	}

	script := false
	if m := psJoinPath.FindStringSubmatch(arg); m != nil {
		var parts []string
		for rest := m[2]; strings.TrimSpace(rest) != ""; {
			a := psStringArg.FindStringSubmatch(rest)
			if a == nil {
				return nil
			}
			parts = append(parts, a[1]+a[2])
			rest = rest[len(a[0]):]
		}
		arg, script = path.Join(parts...), true
	} else {
		arg = strings.Trim(arg, `"'`)
		if loc := psScriptRoot.FindStringIndex(arg); loc != nil {
			arg, script = arg[loc[1]:], true
		}
	}
	arg = strings.ReplaceAll(arg, "\\", "/")
	if arg == "" || strings.ContainsAny(arg, "$`*?~(){}") || strings.HasPrefix(arg, "/") || strings.Contains(arg, ":") {
		return nil
	}
	ext := strings.ToLower(path.Ext(arg))
	named := !strings.Contains(arg, "/") && ext != ".ps1" && ext != ".psm1" && ext != ".psd1" && !strings.HasPrefix(arg, ".")
	if ext == ".ps1" || ext == ".psm1" || ext == ".psd1" {
		arg = arg[:len(arg)-len(ext)]
	}
	var out []Import
	add := func(cleaned string) {
		if cleaned == "" || cleaned == "." || strings.HasPrefix(cleaned, "..") {
			return
		}
		next := imp
		next.Path = cleaned
		out = append(out, next)
	}
	if named {
		// A module by name is found on the module path, not beside the script.
		add(arg)
		return out
	}
	add(path.Join(path.Dir(filePath), arg))
	if !script {
		add(path.Clean(arg))
	}
	return out
}

// psTokens splits a command's text into words, keeping a quoted string whole.
func psTokens(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case quote != 0:
			cur.WriteRune(r)
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
			cur.WriteRune(r)
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}
