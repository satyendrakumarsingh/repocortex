package parse

import (
	"path"
	"regexp"
	"strings"

	ts "github.com/odvcencio/gotreesitter"
)

// HCL (Terraform, OpenTofu, Packer, Terragrunt, Nomad). What the index holds,
// see fixtures/langs/hcl/expected.tsv.
//
// The inferred query tagged every block at every depth and named it by its
// last label, or by its keyword when it has none. In a Terraform file that
// made `ingress`, `tags`, `provisioner "file"` and `locals` definitions (60% of
// the symbols were not), and gave a resource and a variable of the same name
// one name. It is replaced.
//
// A definition is a labelled block at the top level of the file, or an entry of
// a top-level `locals` block. A block nested in another block, and a block with
// no label (`terraform`, `locals`, `moved`, `import`), is configuration of
// something else.
//
// A block is named the way Terraform refers to it, so that two things of one
// name in different roles cannot collide:
//
//	resource "aws_instance" "web"   aws_instance.web      class
//	data "aws_ami" "ubuntu"         data.aws_ami.ubuntu    class
//	module "network"                module.network         package
//	provider "aws"                  provider.aws           package
//	provider "aws" { alias = "w" }  provider.aws.w         package
//	variable "region"               var.region             variable
//	output "id"                     output.id              constant
//	locals { name = ... }           local.name             constant
//	check "health"                  check.health           class
//
// Any other labelled top-level block (Packer `source`, Terragrunt
// `dependency`, Nomad `job`) is its keyword and labels joined by dots, a class.
//
// A query can capture one node as the name and the two labels are two nodes, so
// the block itself is captured as both @name and the definition; the name
// correction registered below receives the text of the block, reads its header
// (or the `name =` of a locals entry) and builds the name.
func init() {
	registerDefinitions("hcl", hclDefinitions+"\n"+hclReferences, true)
	registerNameFix("hcl", hclName)
	tagAdjusters["hcl"] = hclReferenceNames
	registerImportFix("hcl", hclImport)
}

// # References between the blocks of a Terraform module
//
// A Terraform module is a directory: every `.tf` file in it shares one scope, and an
// expression names what it uses by its address, which is what a definition above is
// named by. So a reference is the traversal it is written as, under the name it is
// looked up by:
//
//	var.region                   var.region
//	local.tags                   local.tags
//	data.aws_ami.ubuntu.id       data.aws_ami.ubuntu
//	aws_instance.web.id          aws_instance.web
//	module.net                   module.net
//	module.net.subnet_id         module.net.subnet_id   (the output of the module the block names)
//	provider = aws.west          provider.aws.west
//	region = "x" in a .tfvars    var.region
//
// A first segment that is none of those words and has no underscore (`each`, `count`,
// `self`, `path`, `terraform`, a `for` variable) is not an address and is not read. A
// resource type has an underscore (`aws_instance`, `null_resource`) because it begins
// with its provider's name. What a file reaches through `module.net.x` is found through
// the `source` of the block `module "net"`, which is read as an import row (hclImport):
// the repository directory of a local path, or `remote:` and the source of a registry
// or git module, which is outside the repository.
//
// Only Terraform's files are resolved. Packer, Nomad and Terragrunt write `.hcl` files
// whose traversals mean other things, and the resolver ignores a reference written in
// one (internal/resolve/infer/hcl.go).
const hclReferences = `
(expression (variable_expr (identifier) @_h) . (get_attr (identifier) @name) (#any-of? @_h "var" "local")) @reference.hcl1
(expression (variable_expr (identifier) @_h) . (get_attr (identifier) @name) (#match? @_h "_")) @reference.hcl1
(expression (variable_expr (identifier) @_h) . (get_attr (identifier) @name) (#eq? @_h "module")) @reference.hcl1
(expression (variable_expr (identifier) @_h) . (get_attr) . (get_attr (identifier) @name) (#any-of? @_h "data" "module")) @reference.hcl2
(attribute . (identifier) @_a . (expression (variable_expr (identifier) @_h) . (get_attr (identifier) @name)) (#eq? @_a "provider")) @reference.hclprovider2
(attribute . (identifier) @_a . (expression (variable_expr (identifier) @name) .) (#eq? @_a "provider")) @reference.hclprovider1
(config_file (body (attribute . (identifier) @name) @reference.hcltfvar))
`

// hclReferenceNames gives each reference tag the address it is looked up by, read
// back from the source before the captured name.
func hclReferenceNames(src []byte, tags []ts.Tag) []ts.Tag {
	out := tags[:0]
	for _, t := range tags {
		switch t.Kind {
		case "reference.hcl1", "reference.hcl2":
			back := 1
			if t.Kind == "reference.hcl2" {
				back = 2
			}
			start := int(t.NameRange.StartByte)
			for i := 0; i < back; i++ {
				if start < 2 || src[start-1] != '.' {
					start = -1
					break
				}
				j := start - 1
				for j > 0 && hclIdentChar(src[j-1]) {
					j--
				}
				start = j
			}
			if start < 0 {
				continue
			}
			t.Name = string(src[start:t.NameRange.EndByte])
			t.NameRange.StartByte = uint32(start)
		case "reference.hclprovider1":
			t.Name = "provider." + t.Name
		case "reference.hclprovider2":
			t.Name = "provider." + hclWordBefore(src, int(t.NameRange.StartByte)) + "." + t.Name
		case "reference.hcltfvar":
			t.Name = "var." + t.Name
		}
		out = append(out, t)
	}
	return out
}

func hclIdentChar(b byte) bool {
	return b == '_' || b == '-' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// hclWordBefore is the identifier that ends just before the `.` before pos.
func hclWordBefore(src []byte, pos int) string {
	if pos < 2 || src[pos-1] != '.' {
		return ""
	}
	end := pos - 1
	j := end
	for j > 0 && hclIdentChar(src[j-1]) {
		j--
	}
	return string(src[j:end])
}

// hclImport reads a `module` block into a row, `name=source`, with the alias `include`
// so that the rows of every file of a directory can be read together (the registry
// keeps them as the file's directives): `module.net.x`, written in any file of the
// directory, names an output of the module `net`, which only the file that declares
// the block can say.
//
//	module "net" { source = "./modules/net" }      net=modules/net
//	module "vpc" { source = "terraform-aws-modules/vpc/aws" }
//	                                               vpc=remote:terraform-aws-modules/vpc/aws
//
// The path of a local module is relative to the directory of the file that writes the
// block and is rewritten to the repository directory, without a trailing slash. A
// source that is not a path (a registry address, `git::`, `github.com/...`, `s3::`) is
// kept after `remote:`; a path above the repository's root names nothing in it.
func hclImport(filePath string, imp Import) []Import {
	text := imp.Path
	h := hclHeader.FindStringSubmatch(text)
	if h == nil || h[1] != "module" {
		return nil
	}
	var name string
	if l := hclLabel.FindStringSubmatch(h[2]); l != nil {
		name = l[1]
		if name == "" {
			name = strings.Trim(l[0], `"`)
		}
	}
	src := hclSource.FindStringSubmatch(text)
	if name == "" || src == nil {
		return nil
	}
	source := src[1]
	imp.Alias = ModuleInclude
	if strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../") || source == "." || source == ".." {
		dir := path.Join(path.Dir(filePath), source)
		if strings.HasPrefix(dir, "..") {
			return nil
		}
		imp.Path = name + "=" + dir
		return []Import{imp}
	}
	imp.Path = name + "=remote:" + source
	return []Import{imp}
}

var hclSource = regexp.MustCompile(`(?m)^\s*source\s*=\s*"([^"]*)"`)

// hclLabelled is a top-level block with at least one label. HEAD stands for
// the predicate that selects the block by its keyword.
const hclLabelled = `(config_file (body (block . (identifier) @_kw [(string_lit) (identifier)] (block_start) HEAD) @name @definition.KIND))`

func hclPattern(kind, head string) string {
	return strings.NewReplacer("KIND", kind, "HEAD", head).Replace(hclLabelled)
}

var hclDefinitions = strings.Join([]string{
	hclPattern("class", `(#any-of? @_kw "resource" "data")`),
	hclPattern("package", `(#any-of? @_kw "module" "provider")`),
	hclPattern("variable", `(#eq? @_kw "variable")`),
	hclPattern("constant", `(#eq? @_kw "output")`),
	hclPattern("class", `(#not-any-of? @_kw "resource" "data" "module" "provider" "variable" "output" "locals")`),
	// An entry of a top-level locals block.
	`(config_file (body (block . (identifier) @_kw . (block_start) (body (attribute) @name @definition.constant)) (#eq? @_kw "locals")))`,
}, "\n")

var (
	// hclHeader reads a block's keyword and labels. A label is a quoted string
	// or a bare identifier.
	hclHeader = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_-]*)((?:\s+(?:"[^"]*"|[A-Za-z_][A-Za-z0-9_-]*))*)\s*\{`)
	hclLabel  = regexp.MustCompile(`"([^"]*)"|[A-Za-z_][A-Za-z0-9_-]*`)
	hclEntry  = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_-]*)\s*=`)
	hclAlias  = regexp.MustCompile(`(?m)^\s*alias\s*=\s*"([^"]*)"`)
)

// hclName turns the text of a captured definition into its name.
func hclName(text string) string {
	if m := hclEntry.FindStringSubmatch(text); m != nil {
		return "local." + m[1]
	}
	m := hclHeader.FindStringSubmatch(text)
	if m == nil {
		// A header the pattern cannot read (a comment between the labels):
		// better its first line than the text of the whole block.
		first, _, _ := strings.Cut(text, "\n")
		return strings.TrimSpace(first)
	}
	keyword := m[1]
	var labels []string
	for _, l := range hclLabel.FindAllStringSubmatch(m[2], -1) {
		if l[1] != "" || strings.HasPrefix(l[0], `"`) {
			labels = append(labels, l[1])
		} else {
			labels = append(labels, l[0])
		}
	}
	switch keyword {
	case "resource":
		// The resource type is the first label, and is not a keyword.
	case "variable":
		labels = append([]string{"var"}, labels...)
	case "provider":
		labels = append([]string{"provider"}, labels...)
		if a := hclAlias.FindStringSubmatch(text); a != nil {
			labels = append(labels, a[1])
		}
	default:
		// data, module, output and every other block keep their keyword.
		labels = append([]string{keyword}, labels...)
	}
	return strings.Join(labels, ".")
}
