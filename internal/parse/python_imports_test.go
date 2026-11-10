package parse

import (
	"context"
	"sort"
	"testing"
)

// TestPythonRelativeImportsAreExtracted covers the gap that made Python's
// import tier unreachable in any file whose imports were relative.
//
// Python is a file-is-a-module language, so the resolver consults imports
// BEFORE proximity. A file that declares no imports has no import tier at all
// and falls through to the same-directory tier that ordering exists to demote —
// which is the exact failure docs/accuracy.md measured at 144 wrong
// high-confidence edges from one name.
//
// The assertion is on the SET of paths, not on a count, because a count passes
// just as happily when the wrong three are captured.
func TestPythonRelativeImportsAreExtracted(t *testing.T) {
	src := []byte(`
import os
import numpy as np
from scrapy.utils.test import get_crawler
from .helpers import Helper
from ..pkg.mod import Thing
from . import sibling
`)
	p := NewRegistry().ForFile("probe.py")
	if p == nil {
		t.Fatal("no parser for python")
	}
	res, err := p.Parse(context.Background(), "probe.py", src)
	if err != nil {
		t.Fatal(err)
	}

	got := make([]string, 0, len(res.Imports))
	aliases := map[string]string{}
	for _, imp := range res.Imports {
		got = append(got, imp.Path)
		if imp.Alias != "" {
			aliases[imp.Path] = imp.Alias
		}
	}
	sort.Strings(got)

	// `from . import sibling` is deliberately absent: its module_name is a bare
	// relative_import with no dotted_name, and the imported names that follow are
	// not necessarily modules — `from . import helper_fn` names a function. A
	// path of "." carries no segments and would be dropped by buildImportSet
	// anyway, so capturing it would add a row that can never match.
	want := []string{"helpers", "numpy", "os", "pkg.mod", "scrapy.utils.test"}
	if len(got) != len(want) {
		t.Fatalf("imports = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("imports = %v, want %v", got, want)
		}
	}
	if aliases["numpy"] != "np" {
		t.Errorf("alias for numpy = %q, want np", aliases["numpy"])
	}
}
