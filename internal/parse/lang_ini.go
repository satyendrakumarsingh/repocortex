package parse

// INI. What the index holds, see fixtures/langs/ini/expected.tsv: each
// [section] header is a package named by the section name between the
// brackets, with its surrounding white space removed. A section name
// containing a colon is not extracted: the grammar cannot tell it from a key
// separator.
//
// Keys are not definitions, as in TOML and YAML: a key is a setting, names
// like host or port occur in nearly every file, and listing them as fields
// would fill every name lookup with them. Values and comments are not
// definitions either.
func init() {
	registerDefinitions("ini", iniDefinitions, true)
}

const iniDefinitions = `
(section (section_name (text) @name) @definition.module
  (#not-match? @name ":"))
`
