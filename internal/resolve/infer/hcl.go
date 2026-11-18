package infer

import (
	"path"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// # Terraform: a module is a directory, and a reference names a block of it
//
// Every `.tf` file in a directory is one module, and an expression names what it uses by
// its address: `var.region`, `local.tags`, `data.aws_ami.ubuntu`, `aws_instance.web`,
// `module.net`, `provider.aws.west`. The parser reads each traversal under that address
// (parse.hclReferenceNames) and names each block the same way (parse.hclName), so a
// reference reaches the block of that name that is written in a file of the same
// directory, whichever file that is.
//
//	var.x, local.x, data.t.n, t.n, module.m     the block of that address in this directory
//	module.m.out                                the `output "out"` of the directory the
//	                                            block `module "m"` names as its `source`
//	provider = aws.west                         the provider block, or the provider's own
//	region = "x" in a .tfvars                   the variable `region` of this directory
//
// The `source` of a module block is a row of the file that writes it (parse.hclImport) and
// is read for the whole directory, because `module.net.id` is written in a file other than
// the one that declares `module "net"`. A source that is a path in the repository names the
// directory; a registry address, a git URL and the like are `remote:` and are outside it.
// A resource's type (`aws_instance`) is a provider's, which no file of the repository
// declares: a reference to a resource this directory does not declare is bound to nothing.
//
// Only Terraform's files are read: `.tf` and `.tfvars`. A `.hcl` file is Packer's, Nomad's
// or Terragrunt's, whose traversals mean other things, and a reference in one is ignored.
func isTerraformFile(filePath string) bool {
	switch path.Ext(filePath) {
	case ".tf", ".tfvars":
		return true
	}
	return false
}

func (r *Resolver) resolveHCL(c call) decision {
	name := c.ref.Name
	dir := path.Dir(c.fr.Path)

	if rest, ok := strings.CutPrefix(name, "module."); ok {
		if module, output, hasOutput := strings.Cut(rest, "."); hasOutput {
			return r.hclModuleOutput(c, dir, module, output)
		}
	}

	var here []registry.SymbolIndexEntry
	for _, s := range c.cands {
		if path.Dir(s.FilePath) == dir && isTerraformFile(s.FilePath) && languageOfCandidate(s) == "hcl" {
			here = append(here, s)
		}
	}
	switch len(here) {
	case 0:
		if strings.HasPrefix(name, "provider.") {
			// A provider with no block of its own is the provider's default configuration.
			return externalCall
		}
		return unresolvedCall
	case 1:
		return bound(candidate{here[0], model.ConfidenceHigh, "declared in a file of the same module directory"})
	}
	return ambiguousCall
}

// hclModuleSources are the `source` of every module block of a directory, by the block's name,
// read from the rows of all of its files.
func (r *Resolver) hclModuleSources(dir string) map[string]string {
	if r.hclModules == nil {
		r.hclModules = map[string]map[string]string{}
		if g := r.reachGraphs["hcl"]; g != nil {
			for _, key := range g.keys {
				d := path.Dir(key)
				for _, part := range g.files[key].Parts {
					if name, source, ok := strings.Cut(part, "="); ok {
						if r.hclModules[d] == nil {
							r.hclModules[d] = map[string]string{}
						}
						if _, dup := r.hclModules[d][name]; !dup {
							r.hclModules[d][name] = source
						}
					}
				}
			}
		}
	}
	return r.hclModules[dir]
}

// hclModuleOutput binds `module.m.out` to the output the block `module "m"` names a directory
// for.
func (r *Resolver) hclModuleOutput(c call, dir, module, output string) decision {
	source, ok := r.hclModuleSources(dir)[module]
	if !ok {
		return unresolvedCall
	}
	if strings.HasPrefix(source, "remote:") {
		return externalCall
	}
	var outputs []registry.SymbolIndexEntry
	if c.typesNamed != nil {
		for _, s := range c.typesNamed("output." + output) {
			if path.Dir(s.FilePath) == source && isTerraformFile(s.FilePath) && languageOfCandidate(s) == "hcl" {
				outputs = append(outputs, s)
			}
		}
	}
	switch len(outputs) {
	case 0:
		if r.layout.Known() && !r.layout.HasDir(strings.Split(strings.ToLower(source), "/")) {
			// The directory the source names holds no indexed file.
			return externalCall
		}
		return unresolvedCall
	case 1:
		return bound(candidate{outputs[0], model.ConfidenceHigh, "the output of the module whose source the block names"})
	}
	return ambiguousCall
}
