package parse

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// flagsOf returns the Singleton and Conditional bits of each symbol, by name and
// line, as "name:line" -> "s", "c", "sc" or "".
func flagsOf(res *Result) map[string]string {
	out := map[string]string{}
	for _, s := range res.Symbols {
		f := ""
		if s.Singleton {
			f += "s"
		}
		if s.Conditional {
			f += "c"
		}
		out[s.Name+":"+itoa(s.Range.Start.Line)] = f
	}
	return out
}

func TestRubyRecordsSingletonMethods(t *testing.T) {
	res := parse(t, "a.rb", `class Net
  def self.start
  end

  def start
  end

  class << self
    def open
    end
  end

  def open
  end
end
`)
	got := flagsOf(res)
	for k, want := range map[string]string{
		"start:2": "s", // def self.start
		"start:5": "",  // def start
		"open:9":  "s", // def open under class << self
		"open:13": "",  // def open
	} {
		if g, ok := got[k]; !ok || g != want {
			t.Errorf("%s: flags %q (present %v), want %q; all %v", k, g, ok, want, got)
		}
	}
}

func TestCRecordsDefinitionsUnderAPreprocessorConditional(t *testing.T) {
	res := parse(t, "compat.c", `#ifndef COMPAT_H
#define COMPAT_H
#ifdef _WIN32
int plat(void) { return 1; }
#else
int plat(void) { return 2; }
#endif
int solo(void) { return 0; }
#endif
`)
	got := flagsOf(res)
	for k, want := range map[string]string{
		"plat:4": "c", // #ifdef branch
		"plat:6": "c", // #else branch
		"solo:8": "",  // inside the header guard only
	} {
		if g, ok := got[k]; !ok || g != want {
			t.Errorf("%s: flags %q (present %v), want %q; all %v", k, g, ok, want, got)
		}
	}
}

func TestCppHeaderGuardAndExternCAreNotAlternatives(t *testing.T) {
	res := parse(t, "a.hpp", `#ifndef A_H
#define A_H
#ifdef __cplusplus
extern "C" {
#endif
inline int one(void) { return 1; }
#ifdef __cplusplus
}
#endif
#if defined(FAST)
inline int two(void) { return 2; }
#endif
#endif
`)
	got := flagsOf(res)
	if g, ok := got["one:6"]; !ok || g != "" {
		t.Errorf("one: flags %q (present %v), want none; all %v", g, ok, got)
	}
	if g, ok := got["two:11"]; !ok || g != "c" {
		t.Errorf("two: flags %q (present %v), want conditional; all %v", g, ok, got)
	}
}

// SymbolFlag bits are the on-disk format: a change to a value is a change to
// every existing index.
func TestSymbolFlagBitsAreStable(t *testing.T) {
	if model.SymbolFlagPrivate != 1 || model.SymbolFlagTrait != 2 ||
		model.SymbolFlagSingleton != 16 || model.SymbolFlagConditional != 4 || model.SymbolFlagRefImpl != 8 {
		t.Errorf("symbols.flags bits moved")
	}
	s := model.Symbol{Private: true, Singleton: true, Conditional: true}
	if s.Flags() != 1|4|16 {
		t.Errorf("Flags() = %d, want 21", s.Flags())
	}
}

// callBlocks returns, by "name:line", whether each call reference passes a block.
func callBlocks(res *Result) map[string]bool {
	out := map[string]bool{}
	for _, r := range res.References {
		if isCallTag(r.Kind) {
			out[r.Name+":"+itoa(r.Range.Start.Line)] = r.Block
		}
	}
	return out
}

func TestRubyCallPassesBlock(t *testing.T) {
	res := parse(t, "a.rb", `class Foo
  def run
    patch do
      x
    end
    patch :p1 do
      y
    end
    resource("r") { z }
    plain(1)
    list.each do |i|
      i
    end
  end
end
`)
	got := callBlocks(res)
	want := map[string]bool{
		"patch:3":    true,
		"patch:6":    true,
		"resource:9": true,
		"plain:10":   false,
		"each:11":    true,
	}
	for k, w := range want {
		g, ok := got[k]
		if !ok {
			t.Errorf("no call reference %s; have %v", k, got)
			continue
		}
		if g != w {
			t.Errorf("%s: Block = %v, want %v", k, g, w)
		}
	}
}
