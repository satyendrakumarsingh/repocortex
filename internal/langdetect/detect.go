// Package langdetect decides which grammar reads a file.
//
// The grammar library answers that question from the file name alone, and when
// two grammars claim one extension the first one registered always wins. That
// is wrong often enough to matter: every `.m` file was MATLAB, so an
// Objective-C codebase yielded nothing; every `.h` was C, so the classes in a
// C++ header were lost; every `.fs` was Forth, which has no definition query,
// so F# was skipped outright.
//
// This package sits in front of the library and consults, in order:
//
//  1. the repository's own overrides (index.language_overrides in
//     .repocortex.yml), last matching entry first;
//  2. an exact file name or compound suffix the library knows (Makefile,
//     CMakeLists.txt, .blade.php);
//  3. for an extension more than one language uses, the `#!` line and then the
//     ordered content rules in rules.go, applied to the first HeadBytes of the
//     file, and for `.h` what the neighbouring files are;
//  4. the library's extension table;
//  5. for a name that table does not know, the `#!` line.
//
// Three properties are deliberate.
//
// IT IS DETERMINISTIC. The answer is a function of the path, the first
// HeadBytes of content and the names of the files in the same directory. No
// statistics, no classifier, no dependence on the order files are visited in:
// two runs over one tree must produce one index.
//
// IT SAYS WHEN IT COULD NOT TELL. A file whose extension is ambiguous and whose
// content matches no rule keeps the library's default, and is reported as
// ByAmbiguousExtension so the index can list it instead of presenting a guess
// as a fact.
//
// IT COSTS ALMOST NOTHING. Detection runs for every file in a repository. An
// unambiguous extension is settled by two map lookups and no content is looked
// at. Content rules run only for the extensions in rules.go, over a few
// kilobytes the walker has already read to hash the file.
package langdetect

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"

	"github.com/odvcencio/gotreesitter/grammars"
)

// HeadBytes is how much of a file the content rules may look at.
//
// 8 KiB is one page-cache read and, measured over GitHub Linguist's sample
// tree, enough for every rule here to fire on a file that carries its marker
// at all: licence headers push the first real line down by one to two
// kilobytes, not eight. It is also the window the walker already sniffs for a
// NUL byte, so no file is read further for detection than it was before.
const HeadBytes = 8 << 10

// Method records how a file's language was decided.
//
// The values are stable identifiers, not prose: they are stored in the index
// (files.detected_by) and printed by `repocortex status`, so changing one is a
// schema change.
type Method string

const (
	// ByOverride: an index.language_overrides entry in .repocortex.yml matched.
	ByOverride Method = "override"

	// ByFilename: the exact file name, or a compound suffix, is one the
	// library knows (Makefile, Dockerfile, CMakeLists.txt, .blade.php).
	ByFilename Method = "filename"

	// ByExtension: the extension belongs to one language, or to several of
	// which the plain reading is safe (a `.ts` file that is not Qt XML).
	ByExtension Method = "extension"

	// ByShebang: the `#!` line names an interpreter.
	ByShebang Method = "shebang"

	// ByContent: the extension is shared by several languages and a content
	// rule settled it.
	ByContent Method = "content"

	// ByNeighbours: a `.h` file with no marker of its own, placed by the
	// source files beside it.
	ByNeighbours Method = "neighbours"

	// ByAmbiguousExtension: the extension is shared by several languages, no
	// rule matched, and the library's default was kept. This is the "detected
	// by extension only, ambiguous" case: the language recorded for the file
	// is a guess, and a parse that then yields nothing is most likely a wrong
	// guess rather than an empty file.
	ByAmbiguousExtension Method = "extension_ambiguous"
)

// Result is what detection knew about one file.
type Result struct {
	// Language is the grammar to parse the file with, by the grammar
	// library's name ("cpp", "objc", "fsharp"). Empty when no grammar applies.
	Language string

	// Method is how Language was decided. Empty when nothing was detected at
	// all: an unknown name with no `#!` line.
	Method Method

	// Recognised names what the content is when that language has no grammar
	// here ("coq", "mumps", "mercury"). Language is then empty: the file is
	// skipped rather than fed to a grammar for a different language, which
	// produced junk symbols at best and a parse timeout at worst.
	Recognised string
}

// Ambiguous reports whether Language is a guess from the extension alone.
func (r Result) Ambiguous() bool { return r.Method == ByAmbiguousExtension }

// Neighbours summarises the other files in a file's directory, which is the
// only evidence there is for a header that declares nothing language-specific.
type Neighbours struct {
	C    bool // a .c file
	CPP  bool // a .cc, .cpp, .cxx, .c++, .hpp, .hxx, .ipp, .inl, .tpp or .txx file
	ObjC bool // a .m or .mm file
}

// Observe folds one sibling file name into the summary.
func (n *Neighbours) Observe(name string) {
	switch strings.ToLower(path.Ext(name)) {
	case ".c":
		n.C = true
	case ".cc", ".cpp", ".cxx", ".c++", ".hpp", ".hxx", ".ipp", ".inl", ".tpp", ".txx":
		n.CPP = true
	case ".m", ".mm":
		n.ObjC = true
	}
}

// NoLanguage is the override value that marks matching files as having no
// grammar, so they are recorded and skipped instead of parsed.
const NoLanguage = "none"

// MaxOverrides bounds index.language_overrides.
//
// .repocortex.yml arrives with the repository and is untrusted, and every entry
// is matched against every file. A hand-written list is a handful of lines; one
// that is not is generated or hostile.
const MaxOverrides = 256

// Override routes the files a pattern matches to one language.
type Override struct {
	// Pattern is the glob as written, kept for error messages.
	Pattern string

	// Match reports whether the override applies to a repo-relative slash
	// path. Supplied by the caller so this package stays free of any glob
	// dialect: the walker compiles Pattern with the same matcher it uses for
	// index.include and index.exclude.
	Match func(rel string) bool

	// Language is a grammar name, any name the library accepts for one
	// ("C++", "objective-c", "F#"), or NoLanguage.
	Language string
}

// Detector decides the language of files in one repository. It is immutable
// after New and safe for concurrent use.
type Detector struct {
	overrides []Override // Language already resolved to a grammar name or ""
}

// New builds a Detector. A nil or empty overrides list is the normal case.
//
// An override naming a language no grammar exists for is an error, not a
// no-op: a setting that is accepted and silently ignored is worse than one
// that is refused, because the file keeps being misread and nothing says why.
func New(overrides []Override) (*Detector, error) {
	if len(overrides) > MaxOverrides {
		return nil, fmt.Errorf("index.language_overrides has %d entries, over the %d limit",
			len(overrides), MaxOverrides)
	}
	d := &Detector{}
	var errs []error
	for _, o := range overrides {
		if o.Match == nil {
			errs = append(errs, fmt.Errorf("index.language_overrides: pattern %q is not a usable glob", o.Pattern))
			continue
		}
		name, ok := resolveLanguage(o.Language)
		if !ok {
			errs = append(errs, fmt.Errorf(
				"index.language_overrides: %q names no language RepoCortex has a grammar for (pattern %q); "+
					"`repocortex languages --verbose` lists them, and %q marks files as not to be parsed",
				o.Language, o.Pattern, NoLanguage))
			continue
		}
		d.overrides = append(d.overrides, Override{Pattern: o.Pattern, Match: o.Match, Language: name})
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return d, nil
}

// resolveLanguage maps a name a person would write to a grammar name.
func resolveLanguage(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if strings.EqualFold(name, NoLanguage) {
		return "", true
	}
	if name == "" {
		return "", false
	}
	if e := grammars.DetectLanguageByName(name); e != nil {
		return e.Name, true
	}
	// The library's alias table follows Linguist's display names and misses a
	// few spellings people reach for first.
	if g, ok := languageAliases[strings.ToLower(name)]; ok {
		return g, true
	}
	return "", false
}

var languageAliases = map[string]string{
	"objective-c++": "objc",
	"objectivec":    "objc",
	"obj-c":         "objc",
	"objc++":        "objc",
	"systemverilog": "verilog",
	"csharp":        "c_sharp",
	"plsql":         "sql",
	"plpgsql":       "sql",
	"tsql":          "sql",
}

// NeedsContent reports whether Detect can use the head of this file.
//
// False for an unambiguous extension, which is nearly every file in a
// repository; the walker then skips copying the head out of its read buffer.
func (d *Detector) NeedsContent(rel string) bool {
	base := path.Base(rel)
	if _, ok := d.override(rel); ok {
		return false
	}
	ext := strings.ToLower(path.Ext(base))
	if _, ruled := extensionRules()[ext]; ruled {
		return true
	}
	// An unknown name: only a `#!` line can place it.
	return libraryName(base) == ""
}

// Detect decides the language of one file.
//
// rel is the repo-relative slash path. head is the first HeadBytes of the
// file, or all of it when shorter; nil means the content was not read (a file
// the walker skipped), and an ambiguous extension is then reported as such.
// neighbours is called at most once, and only for a `.h` file whose content
// settles nothing; it may be nil.
func (d *Detector) Detect(rel string, head []byte, neighbours func() Neighbours) Result {
	if lang, ok := d.override(rel); ok {
		return Result{Language: lang, Method: ByOverride}
	}

	base := path.Base(rel)
	ext := strings.ToLower(path.Ext(base))
	byName := libraryName(base)
	byExt := extensionDefault(ext)

	// An exact file name or a compound suffix beats the bare extension, in the
	// library and here: Makefile.inc is a makefile, not whatever `.inc` is.
	if byName != "" && byName != byExt {
		return Result{Language: byName, Method: ByFilename}
	}

	rule, ruled := extensionRules()[ext]
	if !ruled {
		if byName != "" {
			return Result{Language: byName, Method: ByExtension}
		}
		if lang := shebangLanguage(head); lang != "" {
			return Result{Language: lang, Method: ByShebang}
		}
		return Result{}
	}

	if rule.stem {
		// A NuGet source transform is named Foo.cs.pp: the inner extension
		// says what the text is.
		if inner := libraryName(strings.TrimSuffix(base, path.Ext(base))); inner != "" {
			return Result{Language: inner, Method: ByExtension}
		}
	}

	if head == nil {
		return rule.unsettled(byName)
	}
	head = bytes.TrimPrefix(head, utf8BOM)
	if len(bytes.TrimSpace(head)) == 0 && !rule.noGrammar {
		// Nothing to read and nothing to parse: the default cannot be wrong
		// about an empty file, so it is not reported as a guess.
		return Result{Language: byName, Method: methodFor(byName, ByExtension)}
	}

	if lang := shebangLanguage(head); lang != "" && rule.names(lang) {
		return Result{Language: lang, Method: ByShebang}
	}
	src := source{raw: head}
	if rule.prepare != nil {
		src.raw = rule.prepare(head)
	}
	for i := range rule.content {
		c := &rule.content[i]
		if c.match(&src) {
			return Result{Language: c.language, Method: ByContent, Recognised: c.recognised}
		}
	}
	if rule.neighbours != nil && neighbours != nil {
		if lang, ok := rule.neighbours(neighbours()); ok {
			return Result{Language: lang, Method: ByNeighbours}
		}
	}
	return rule.unsettled(byName)
}

// FromName detects by file name alone: overrides, content and neighbours are
// not consulted. For callers that have a path and nothing else.
func FromName(name string) string {
	return libraryName(path.Base(name))
}

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

func methodFor(language string, m Method) Method {
	if language == "" {
		return ""
	}
	return m
}

// override returns the language of the last override matching rel.
//
// Last, as in .gitattributes: a broad rule is written first and narrowed by
// the lines after it.
func (d *Detector) override(rel string) (string, bool) {
	if d == nil {
		return "", false
	}
	for i := len(d.overrides) - 1; i >= 0; i-- {
		if d.overrides[i].Match(rel) {
			return d.overrides[i].Language, true
		}
	}
	return "", false
}

// libraryName is the grammar library's answer for a file name, extended by
// the few extensions it has no entry for.
//
// The library does not fold case consistently: `legacy.C` is found and
// `solver.F90` is not, so preprocessed Fortran was skipped as a file of no
// known type. An extension that fails as written is tried again in lower
// case. The two extensions whose upper-case form means something else by
// convention are settled first.
func libraryName(base string) string {
	ext := path.Ext(base)
	if g, ok := upperCaseExtensions[ext]; ok {
		return g
	}
	if e := grammars.DetectLanguage(base); e != nil {
		return e.Name
	}
	lower := strings.ToLower(ext)
	if g, ok := extraExtensions[lower]; ok {
		return g
	}
	if lower != ext {
		if e := grammars.DetectLanguage("x" + lower); e != nil {
			return e.Name
		}
	}
	return ""
}

// upperCaseExtensions are the extensions that name a different language in
// upper case than in lower: `.C` and `.H` are C++ where `.c` and `.h` are C.
var upperCaseExtensions = map[string]string{
	".C": "cpp",
	".H": "cpp",
}

// extraExtensions maps extensions the library does not know to a grammar.
//
// PL/SQL package specifications and bodies and the dialect-named SQL
// extensions were skipped as "no grammar for this file type" although a SQL
// grammar is compiled in.
var extraExtensions = map[string]string{
	".pkb":   "sql",
	".pks":   "sql",
	".plsql": "sql",
	".pgsql": "sql",
	".vh":    "verilog",
}

// extDefaults caches what the library says for a bare extension, so that the
// "was this a file-name match or an extension match" question costs one map
// read per file rather than a second walk of the library's tables.
var extDefaults sync.Map // ext (lower case, with dot) -> grammar name

func extensionDefault(ext string) string {
	if ext == "" {
		return ""
	}
	if v, ok := extDefaults.Load(ext); ok {
		return v.(string) //nolint:errcheck,forcetypeassert // only strings are stored
	}
	name := libraryName("x" + ext)
	extDefaults.Store(ext, name)
	return name
}
