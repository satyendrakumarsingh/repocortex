package parse

import (
	"context"
	"testing"
)

func importPaths(res *Result) []string {
	out := make([]string, 0, len(res.Imports))
	for _, i := range res.Imports {
		out = append(out, i.Path)
	}
	return out
}

func hasImport(res *Result, path string) bool {
	for _, i := range res.Imports {
		if i.Path == path {
			return true
		}
	}
	return false
}

// TestImportExtraction covers the languages the resolver will target first.
// Imports are what let a binding be high-confidence rather than a name match,
// so a regression here silently degrades every downstream edge.
func TestImportExtraction(t *testing.T) {
	cases := []struct {
		file string
		src  string
		want []string
	}{
		{
			"service.go",
			`package pay

import (
	"context"
	"fmt"

	"github.com/pay/repository"
	notify "github.com/pay/notification"
)

func A() {}`,
			[]string{"context", "fmt", "github.com/pay/repository", "github.com/pay/notification"},
		},
		{
			"Service.java",
			`package com.pay;

import com.pay.repository.TransactionRepository;
import java.util.List;

class A {}`,
			[]string{"com.pay.repository.TransactionRepository", "java.util.List"},
		},
		{
			"service.py",
			`import os
from app.service import TransactionService
`,
			[]string{"os", "app.service"},
		},
		{
			"service.ts",
			`import { TransactionService } from "./service";
import * as fs from "fs";`,
			[]string{"./service", "fs"},
		},
		{
			"service.rb",
			`require "json"
require_relative "./helpers"

class TransactionService
  def initialize; end
end
`,
			[]string{"json", "./helpers"},
		},
		{
			"Service.php",
			`<?php
namespace App;

use App\Repository\TransactionRepository;
use App\Notification\NotificationService;

require 'bootstrap.php';
include 'config.php';

class TransactionService {}
`,
			[]string{"App\\Repository\\TransactionRepository", "App\\Notification\\NotificationService", "bootstrap.php", "config.php"},
		},
		{
			"Service.cs",
			`using System;
using System.Collections.Generic;
using Pay = Pay.Repository;

namespace Pay {
    class TransactionService {}
}
`,
			[]string{"System", "System.Collections.Generic"},
		},
		{
			"Service.kt",
			`package pay

import pay.repository.TransactionRepository
import pay.notification.NotificationService

class TransactionService
`,
			[]string{"pay.repository.TransactionRepository", "pay.notification.NotificationService"},
		},
		{
			// The scala grammar has no node spanning a whole dotted import
			// path (see the comment on the scala entry in queries.go), so
			// what is extractable is the trailing symbol name of a plain
			// import and each name in a `{...}` selector import. A `_`
			// wildcard import (pay.util._) yields nothing, which is the
			// documented gap.
			"Service.scala",
			`package pay

import pay.repository.TransactionRepository
import pay.notification.{NotificationService, Alerter}
import pay.util._

class TransactionService
`,
			[]string{"TransactionRepository", "NotificationService", "Alerter"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			res := parse(t, tc.file, tc.src)
			for _, want := range tc.want {
				if !hasImport(res, want) {
					t.Errorf("missing import %q; got %v", want, importPaths(res))
				}
			}
		})
	}
}

// TestImportQuotesAreStripped: a path stored as `"fmt"` would never match a
// package name during resolution.
func TestImportQuotesAreStripped(t *testing.T) {
	res := parse(t, "x.go", "package p\nimport \"context\"\n")
	for _, i := range res.Imports {
		if len(i.Path) > 0 && (i.Path[0] == '"' || i.Path[0] == '`' || i.Path[0] == '\'') {
			t.Errorf("import path retains quoting: %q", i.Path)
		}
	}
	if !hasImport(res, "context") {
		t.Errorf("expected unquoted 'context'; got %v", importPaths(res))
	}
}

func TestImportAliasCaptured(t *testing.T) {
	res := parse(t, "x.go", `package p

import notify "github.com/pay/notification"
`)
	var found bool
	for _, i := range res.Imports {
		if i.Path == "github.com/pay/notification" {
			found = true
			if i.Alias != "notify" {
				t.Errorf("alias = %q, want notify", i.Alias)
			}
		}
	}
	if !found {
		t.Errorf("aliased import not captured; got %v", importPaths(res))
	}
}

// TestImportsAreDeduplicated guards the overlapping Go patterns: the aliased
// and unaliased import_spec rules both match an aliased import.
func TestImportsAreDeduplicated(t *testing.T) {
	res := parse(t, "x.go", `package p

import (
	alias "github.com/a/b"
	"github.com/c/d"
)
`)
	seen := map[string]int{}
	for _, i := range res.Imports {
		seen[i.Path]++
	}
	for path, n := range seen {
		if n > 1 {
			t.Errorf("import %q appears %d times; deduplication failed", path, n)
		}
	}
}

func TestImportsDeterministic(t *testing.T) {
	src := `package p

import (
	"zebra"
	"alpha"
	"middle"
)
`
	first := importPaths(parse(t, "x.go", src))
	for i := 0; i < 10; i++ {
		got := importPaths(parse(t, "x.go", src))
		if len(got) != len(first) {
			t.Fatalf("run %d: count changed", i)
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("run %d: order changed\n first=%v\n got  =%v", i, first, got)
			}
		}
	}
}

func TestNoImportsIsNotAnError(t *testing.T) {
	res := parse(t, "x.go", "package p\n\nfunc A() {}\n")
	if len(res.Imports) != 0 {
		t.Errorf("expected no imports, got %v", importPaths(res))
	}
	if res.Status == StatusFailed {
		t.Error("a file without imports must not be a failure")
	}
}

// TestMalformedImportsDoNotFail: a half-typed import in an editor buffer is a
// normal state for the file watcher to encounter.
func TestMalformedImportsDoNotFail(t *testing.T) {
	p := parserFor(t, "x.go")
	for _, src := range []string{
		"package p\nimport (",
		"package p\nimport \"unterminated",
		"package p\nimport ( \"a\" \"b\"",
	} {
		res, err := p.Parse(context.Background(), "x.go", []byte(src))
		if err != nil {
			t.Fatalf("run-level error on malformed import: %v", err)
		}
		if res == nil {
			t.Fatal("nil result")
		}
	}
}

// importPathsFor maps a language name to a probe filename and returns the
// import paths extracted from src. Reuses the parse/importPaths harness
// above rather than adding a second one.
func importPathsFor(t *testing.T, lang, src string) []string {
	t.Helper()
	files := map[string]string{
		"typescript": "probe.ts",
		"tsx":        "probe.tsx",
		"javascript": "probe.js",
	}
	file, ok := files[lang]
	if !ok {
		t.Fatalf("importPathsFor: no probe file for language %q", lang)
	}
	return importPaths(parse(t, file, src))
}

// containsPath reports whether want is present in paths.
func containsPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

// A barrel file re-exports its package's public surface. Without an edge for
// `export ... from`, the barrel imports nothing, so every consumer that goes
// through it dead-ends — which in a frontend package is most consumers.
func TestReExportsProduceImportEdges(t *testing.T) {
	cases := []struct {
		lang string
		src  string
		want []string
	}{
		{
			lang: "typescript",
			src: `export { Button } from './button'
export * from './icons'
export type { Props } from './types'
`,
			want: []string{"./button", "./icons", "./types"},
		},
		{
			lang: "tsx",
			src: `export { Button } from './button'
export * from './icons'
`,
			want: []string{"./button", "./icons"},
		},
		{
			lang: "javascript",
			src: `export { handler } from './handler'
export * from './util'
`,
			want: []string{"./handler", "./util"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.lang, func(t *testing.T) {
			paths := importPathsFor(t, tc.lang, tc.src)
			for _, want := range tc.want {
				if !containsPath(paths, want) {
					t.Errorf("%s: no import edge for %q; got %v", tc.lang, want, paths)
				}
			}
		})
	}
}

// A local re-export names no module and must produce no edge.
func TestLocalReExportProducesNoImportEdge(t *testing.T) {
	paths := importPathsFor(t, "typescript", "const x = 1\nexport { x }\n")
	if len(paths) != 0 {
		t.Errorf("export without a from clause must produce no import edge; got %v", paths)
	}
}
