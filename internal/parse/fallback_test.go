package parse

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// The line-shape fallback (fallback.go).
//
// Measured before it was enabled, on real code copied from this machine (no
// fixture), with the shapes of every language that has a definition keyword:
//
//   language    files  with ERROR  lines in ERROR  recovered  wrong
//   perl         1200         105      8879/445559         53      19 (3 in POD
//                                                                   after __END__,
//                                                                   16 forward
//                                                                   declarations)
//   swift         150         117     48309/55989       2144       0
//   kotlin        300          19      1499/66070          0       -
//   rust          300          29        29/103543         0       -
//   powershell     50           9       342/4662           1       0
//   bash          200           4         4/21957          0       -
//   go            300           1         1/63259          0       -
//   tsx           200           5         9/36611          0       -
//   python, ruby, javascript, typescript, java, c_sharp, php, lua: no file
//   with an ERROR node.
//
// Perl was enabled after its two kinds of wrong line were excluded (POD and
// __END__ are dead lines; a bodyless `sub name;` is not a definition): 34
// recovered, 34 correct. Swift, with attributes and `class func` read: 2407
// recovered, none wrong and none a duplicate of a definition the grammar
// found. The Swift corpus is the SDK's .swiftinterface files, which are Swift
// declarations the grammar cannot read (bodyless members); no Swift source
// file was on the machine. PowerShell's one recovery is too little evidence.
// Every other language lost nothing a keyword line would find.

func shapeParse(t *testing.T, language, src string) *Result {
	t.Helper()
	p, err := parserForLanguage(language)
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Parse(context.Background(), "x", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

type recovered struct {
	line, end int
	kind      model.EntityKind
	name      string
}

func recoveredOf(res *Result) []recovered {
	var out []recovered
	for _, s := range res.Symbols {
		if s.Confidence == model.ConfidenceMedium {
			out = append(out, recovered{s.Range.Start.Line, s.Range.End.Line, s.Kind, s.Name})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].line < out[j].line })
	return out
}

func sameRecovered(t *testing.T, got, want []recovered) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("recovered %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("recovered %v, want %v", got[i], want[i])
		}
	}
}

// One substitution the Perl grammar cannot read (`s/.../$x->()/e`) takes the
// subs after it into an ERROR. They come back by line shape, marked medium;
// a comment, a forward declaration and the example code in the POD after
// __END__ do not.
func TestLineShapePerl(t *testing.T) {
	src := `package Text::Box;
use strict;

sub new { bless \(my $s = $_[1]), $_[0] }

sub swap {
  if (ref($_[2]) eq 'CODE') {
    ${$_[0]} =~ s/$_[1]/$_[2]->()/e;
  }
  ${$_[0]};
}

sub size { CORE::length ${$_[0]} }

# sub commented { 1 }
sub later;

sub trim {
  my $self = shift;
  return $self;
}

1;
__END__

=head1 EXAMPLE

  sub in_docs {
    return 1;
  }

=cut
`
	res := shapeParse(t, "perl", src)
	if res.Status != StatusPartial {
		t.Fatalf("status %s: the snippet no longer breaks the grammar, so it tests nothing", res.Status)
	}
	sameRecovered(t, recoveredOf(res), []recovered{
		{6, 11, model.KindFunction, "swap"}, // ends where its braces close
		{13, 13, model.KindFunction, "size"},
	})
	for _, s := range res.Symbols {
		if s.Name == "new" && s.Confidence != model.ConfidenceHigh {
			t.Errorf("new was found by the grammar and must stay high, got %s", s.Confidence)
		}
	}
	if res.RecoveredByShape != 2 || res.Err == nil ||
		!strings.Contains(res.Err.Error(), "2 definitions recovered by line shape") {
		t.Errorf("the parse error must say what was recovered: %d, %v", res.RecoveredByShape, res.Err)
	}
}

// Members the Swift grammar cannot read (bodyless declarations, as Apple's
// interface files write them) come back by line shape. A comment, a block
// comment and a string are not read; a function in a type is a method and
// one at file level a function.
func TestLineShapeSwift(t *testing.T) {
	src := `public struct Meter {
  public enum Mode {
    case fast
    public static func == (a: Mode, b: Mode) -> Bool
    public func hash(into hasher: inout Hasher)
    public var hashValue: Int {
      get
    }
  }
  // func commented() {}
  /* func inBlock() {} */
  public let label = "func inString() {}"
  public func reset(to value: Int) -> Int
  @inlinable public static func zero() -> Meter
  public class func make() -> Meter
  public struct Reading {
    public func value() -> Int
  }
}
public func freeMeter() -> Meter
`
	res := shapeParse(t, "swift", src)
	sameRecovered(t, recoveredOf(res), []recovered{
		{5, 6, model.KindMethod, "hash"},
		{14, 14, model.KindMethod, "zero"},
		{15, 15, model.KindMethod, "make"},
		{16, 16, model.KindClass, "Reading"},
		{17, 17, model.KindMethod, "value"},
		{20, 20, model.KindFunction, "freeMeter"},
	})
}

// A language without a rule gets nothing from the pass, however broken.
func TestLineShapeOnlyForListedLanguages(t *testing.T) {
	res := shapeParse(t, "python", "def ok():\n    pass\n\nx = (1,\n\ndef lost():\n    return 1\n")
	if res.Status != StatusPartial {
		t.Fatalf("status %s: the snippet no longer breaks the grammar", res.Status)
	}
	if got := recoveredOf(res); len(got) != 0 {
		t.Errorf("python has no line-shape rule, recovered %v", got)
	}
}

// The bar the fallback was enabled on: it adds nothing to any conformance
// fixture. TestLanguageConformance fails on any extra symbol too; this says
// which mechanism added it.
func TestLineShapeAddsNothingToFixtures(t *testing.T) {
	for _, language := range languageFixtures(t) {
		dir := filepath.Join(langFixtureRoot, language)
		p, err := parserForLanguage(language)
		if err != nil {
			continue
		}
		_ = filepath.WalkDir(dir, func(path string, e os.DirEntry, err error) error {
			if err != nil || e.IsDir() || strings.HasSuffix(path, ".md") || strings.HasSuffix(path, ".tsv") {
				return err
			}
			src, err := os.ReadFile(path) //nolint:gosec // test fixture path
			if err != nil {
				return err
			}
			res, err := p.Parse(context.Background(), path, src)
			if err != nil {
				return err
			}
			for _, r := range recoveredOf(res) {
				t.Errorf("%s: line shape recovered %v", path, r)
			}
			return nil
		})
	}
}

func TestMatchShape(t *testing.T) {
	perl, swift := lineShapeRules["perl"], lineShapeRules["swift"]
	cases := []struct {
		rule shapeRule
		line string
		name string // "" for no match
	}{
		{perl, "sub name {", "name"},
		{perl, "  sub Foo::bar ($$) {", "Foo::bar"},
		{perl, "sub name", "name"},
		{perl, "sub name;", ""},
		{perl, "sub name ($$);  # forward", ""},
		{perl, "sub {", ""},
		{perl, "my $x = sub {", ""},
		{perl, "package Foo::Bar;", "Foo::Bar"},
		{swift, "public func run(_ x: Int) {", "run"},
		{swift, "@objc @available(iOS 13, *) open func run() {", "run"},
		{swift, "final class Foo<T>: Bar {", "Foo"},
		{swift, "public class func make() -> Self", "make"},
		{swift, "class var shared: Foo", ""},
		{swift, "func(x)", ""},
		{swift, "let f = func", ""},
		{swift, "extension Foo {", ""},
		{swift, "@available(iOS 13", ""},
	}
	for _, c := range cases {
		got, ok := matchShape(c.rule, []byte(c.line))
		if !ok {
			got.name = ""
		}
		if got.name != c.name {
			t.Errorf("%q: name %q, want %q", c.line, got.name, c.name)
		}
	}
}

func TestPerlDeadLines(t *testing.T) {
	lines := strings.Split("sub a {}\n=pod\nsub b {}\n=cut\nsub c {}\n__END__\nsub d {}", "\n")
	b := make([][]byte, len(lines))
	for i, l := range lines {
		b[i] = []byte(l)
	}
	got := perlDeadLines(b)
	want := []bool{false, true, true, true, false, true, true}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d %q: dead %v, want %v", i+1, lines[i], got[i], want[i])
		}
	}
}
