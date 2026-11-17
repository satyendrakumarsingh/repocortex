package gotypes

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/resolve/scip"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Detail is written to Evidence.Detail of every edge this package produces, so
// that an answer says where its proof came from.
const Detail = "go/types (built in)"

// Tool is the provenance recorded on the facts.
var Tool = scip.Tool{Name: "go/types"}

// ObjRef names a declared object: where it is declared and what it is called.
// File is repo-relative with forward slashes; Line and Col are 1-based and
// 0-based respectively, as the rest of RepoCortex stores them.
type ObjRef struct {
	File string
	Line int
	Col  int
	Name string
	// Recv is the receiver type's name for a method, "" for everything else.
	Recv string
	Kind model.EntityKind
}

// FQN is the identity of the declaration within one Result.
func (o ObjRef) FQN() string {
	q := o.Name
	if o.Recv != "" {
		q = o.Recv + "." + o.Name
	}
	return fmt.Sprintf("go/types:%s:%d:%s", o.File, o.Line, q)
}

// Qualified is Type.method for a method and the bare name otherwise.
func (o ObjRef) Qualified() string {
	if o.Recv != "" {
		return o.Recv + "." + o.Name
	}
	return o.Name
}

// Use is one place a repository-declared object is used.
type Use struct {
	// File, Line and Col locate the identifier (Col is 0-based).
	File string
	Line int
	Col  int

	// Obj is the declaration the identifier resolves to. For an instantiated
	// generic it is the declaration, not the instance.
	Obj ObjRef

	// Encl is the declared function, method or package-level variable that
	// contains the use; a function literal belongs to the named declaration
	// that holds it.
	Encl ObjRef

	// Call is true when the identifier is the callee of a call expression.
	// Taken from the syntax tree this package holds, never from a parser's
	// line table.
	Call bool

	// Construct is true when the identifier names the type of a conversion or
	// a composite literal.
	Construct bool
}

// Rel is the edge kind the fact supports: CALLS for a callee that is a
// function or method, INSTANTIATES for a conversion or composite construction
// of a type, REFERENCES for every other use.
func (u Use) Rel() model.EdgeKind {
	switch {
	case u.Construct && u.Obj.Kind == model.KindClass:
		return model.EdgeInstantiates
	case u.Call && (u.Obj.Kind == model.KindFunction || u.Obj.Kind == model.KindMethod):
		return model.EdgeCalls
	}
	return model.EdgeReferences
}

// MethodImpl pairs a concrete method with the interface method it implements.
type MethodImpl struct {
	Concrete ObjRef
	Iface    ObjRef
}

// Impl records that a named type implements an interface declared in the
// repository.
type Impl struct {
	Type  ObjRef
	Iface ObjRef
	// PointerOnly is true when only *Type has the method set, not Type.
	PointerOnly bool
	// Methods are the interface's methods that Type declares itself; a method
	// promoted from an embedded type is not listed.
	Methods []MethodImpl
}

// Callee is what the compiler knows about one call expression's callee
// identifier, whether or not the callee is a node of the graph. It exists so
// that a heuristic edge the compiler contradicts can be withdrawn: the
// compiler resolved every call named Name on this line, and none is the
// heuristic's target.
type Callee struct {
	Line, Col int
	Name      string
	// Known is true when the checker resolved the identifier (to anything: a
	// local, a field, a standard-library function). False means it could not
	// say, and no conclusion may be drawn from this call.
	Known bool
	// Dst is the FQN of the repository declaration it resolves to, or "".
	Dst string
}

// FileFact is everything known about one repository file.
type FileFact struct {
	Path    string
	Defs    []ObjRef
	Uses    []Use
	Callees []Callee
}

// Result is one Check's facts and its report.
type Result struct {
	Files []FileFact
	Impls []Impl
	Stats Stats
	// TypeErrors are the hard type errors in repository files, when
	// Options.CollectErrors was set.
	TypeErrors []TypeError
}

// TypeError is one hard type error in a repository file.
type TypeError struct {
	Path string // repo-relative, slash-separated
	Line int
	Col  int
	Msg  string
}

// Stats is what a run did, for the summary block and for the cost of the run.
type Stats struct {
	GOOS, GOARCH string
	// Goroot is where the standard library was read from; "" means none was
	// found and every standard-library import was a stub.
	Goroot   string
	ModCache string

	Modules  int
	Packages int // directories checked

	// Every checked package is in exactly one of these.
	Complete     int // no errors, every import real
	WithErrors   int // type errors, every import real
	WithStubs    int // at least one import was a stub
	SkippedParse int // a file of the package did not parse

	NoModule     int // directories with Go files and no go.mod
	FilesChecked int
	// FilesRejected are files the host platform's build constraints exclude.
	// They are not checked and keep their heuristic edges; nothing is claimed
	// about other platforms.
	FilesRejected int
	CgoPackages   int

	Dependencies     int // dependency and standard-library packages loaded from source
	DependencyErrors int

	MissingImports []string // sorted import paths replaced by stubs
	MissingCount   int
	ErrorSamples   []string

	Defs          int
	Uses          int
	Calls         int
	Constructs    int
	External      int // uses of the standard library or a dependency
	WithheldTaint int // selections withheld because an embedded type is unknown
	NoEnclosing   int // uses with no declared function or variable around them

	Implementations       int
	ImplementationMethods int

	// Aborted is non-empty when a ceiling stopped the run early.
	Aborted    string
	PeakHeapMB int
	Elapsed    time.Duration
}

func (s *Stats) tally(u *unit) {
	switch {
	case u.parseFailed:
	case u.stubbed:
		s.WithStubs++
	case u.errors > 0:
		s.WithErrors++
	default:
		s.Complete++
	}
	if u.cgo {
		s.CgoPackages++
	}
}

func (s *Stats) finish(missing map[string]struct{}) {
	s.MissingCount = len(missing)
	s.MissingImports = s.MissingImports[:0]
	for p := range missing {
		s.MissingImports = append(s.MissingImports, p)
	}
	sort.Strings(s.MissingImports)
}

// FirstMissing renders up to n of the missing import paths.
func (s Stats) FirstMissing(n int) string {
	m := s.MissingImports
	if len(m) > n {
		m = m[:n]
	}
	return strings.Join(m, ", ")
}

// builder accumulates facts per file.
type builder struct {
	files map[string]*FileFact
	impls []Impl
}

func newBuilder() *builder { return &builder{files: map[string]*FileFact{}} }

func (b *builder) file(path string) *FileFact {
	f := b.files[path]
	if f == nil {
		f = &FileFact{Path: path}
		b.files[path] = f
	}
	return f
}

func (b *builder) result() *Result {
	paths := make([]string, 0, len(b.files))
	for p := range b.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	res := &Result{Impls: b.impls}
	for _, p := range paths {
		f := b.files[p]
		sort.SliceStable(f.Defs, func(i, j int) bool {
			a, c := f.Defs[i], f.Defs[j]
			if a.Line != c.Line {
				return a.Line < c.Line
			}
			return a.Qualified() < c.Qualified()
		})
		sort.SliceStable(f.Uses, func(i, j int) bool {
			a, c := f.Uses[i], f.Uses[j]
			if a.Line != c.Line {
				return a.Line < c.Line
			}
			if a.Col != c.Col {
				return a.Col < c.Col
			}
			return a.Obj.FQN() < c.Obj.FQN()
		})
		res.Files = append(res.Files, *f)
	}
	sort.SliceStable(res.Impls, func(i, j int) bool {
		a, c := res.Impls[i], res.Impls[j]
		if a.Type.FQN() != c.Type.FQN() {
			return a.Type.FQN() < c.Type.FQN()
		}
		return a.Iface.FQN() < c.Iface.FQN()
	})
	return res
}
