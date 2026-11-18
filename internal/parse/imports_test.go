package parse

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
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
			// `use` paths are written with dots, the form the resolver splits.
			[]string{"App.Repository.TransactionRepository", "App.Notification.NotificationService", "bootstrap.php", "config.php"},
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
			// path, so scalaImports (lang_scala.go) reads the siblings. A
			// selector import is one row per name, and a `_` wildcard is the
			// package.
			"Service.scala",
			`package pay

import pay.repository.TransactionRepository
import pay.notification.{NotificationService, Alerter}
import pay.util._

class TransactionService
`,
			[]string{"pay.repository.TransactionRepository", "pay.notification.NotificationService", "pay.notification.Alerter", "pay.util"},
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

// parseAs parses source as a named grammar, for the files whose extension does
// not say it (an Objective-C .m is also a MATLAB one).
func parseAs(t *testing.T, language, path, src string) *Result {
	t.Helper()
	res, err := NewRegistry().ForLanguage(language).Parse(context.Background(), path, []byte(src))
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return res
}

func importAlias(res *Result, path string) (string, bool) {
	for _, i := range res.Imports {
		if i.Path == path {
			return i.Alias, true
		}
	}
	return "", false
}

// A Swift import names a module and no file. Every form reduces to the module,
// and a framework is recorded like any other: it is the resolver, which knows
// the repository's directories, that finds no module of that name.
func TestSwiftImports(t *testing.T) {
	res := parse(t, "Sources/App/Cart.swift", `import Foundation
import UIKit
@testable import Core
import struct Billing.Money
import Shop.Models

final class Cart {}
`)
	for _, want := range []string{"Foundation", "UIKit", "Core", "Billing", "Shop"} {
		if !hasImport(res, want) {
			t.Errorf("missing import %q; got %v", want, importPaths(res))
		}
	}
	if hasImport(res, "Billing.Money") || hasImport(res, "Shop.Models") {
		t.Errorf("a declaration or a submodule was recorded as the module: %v", importPaths(res))
	}
}

// A Dart URI names a file. The parser rewrites it to the file's repository
// path without the extension, resolving a relative one against the importing
// file and a package: one against lib/, and drops the SDK.
func TestDartImports(t *testing.T) {
	res := parse(t, "lib/src/cart.dart", `import 'dart:io';
import 'package:flutter/material.dart';
import 'package:shop/src/util.dart' as u;
import '../pricing.dart' show discount, Tax;
import 'models/item.dart';
export 'api.dart';
part 'cart.g.dart';

class Cart {}
`)
	for _, want := range []string{"lib/material", "lib/src/util", "lib/pricing", "lib/src/models/item", "lib/src/api", "lib/src/cart.g"} {
		if !hasImport(res, want) {
			t.Errorf("missing import %q; got %v", want, importPaths(res))
		}
	}
	for _, p := range importPaths(res) {
		if p == "dart:io" || p == "io" || strings.HasSuffix(p, ".dart") {
			t.Errorf("import %q was not rewritten", p)
		}
	}
	if alias, ok := importAlias(res, "lib/src/util"); !ok || alias != "u" {
		t.Errorf("`as u` alias = %q, want u", alias)
	}
	// An export and a part name a file without importing it: the row says so.
	if alias, ok := importAlias(res, "lib/src/api"); !ok || alias != ModuleExport {
		t.Errorf("`export` alias = %q, want %q", alias, ModuleExport)
	}
	if alias, ok := importAlias(res, "lib/src/cart.g"); !ok || alias != ModuleInclude {
		t.Errorf("`part` alias = %q, want %q", alias, ModuleInclude)
	}
	if alias, ok := importAlias(res, "lib/src/models/item"); !ok || alias != "" {
		t.Errorf("a plain import has alias %q, want none", alias)
	}
}

// A quoted include names a header of the repository; a framework header and an
// `@import` module are not repository code and are not recorded. The path is
// kept as written and also joined to the importing file's directory, because a
// quoted include is looked for there first.
func TestObjCImports(t *testing.T) {
	res := parseAs(t, "objc", "Shop/Cart.m", `#import <Foundation/Foundation.h>
#import "Models/Item.h"
#include "util.h"
@import UIKit;

@implementation Cart
@end
`)
	for _, want := range []string{"Models/Item", "Shop/Models/Item", "util", "Shop/util"} {
		if !hasImport(res, want) {
			t.Errorf("missing import %q; got %v", want, importPaths(res))
		}
	}
	for _, p := range importPaths(res) {
		if strings.Contains(p, "Foundation") || strings.Contains(p, "UIKit") || strings.HasSuffix(p, ".h") {
			t.Errorf("unexpected import %q", p)
		}
	}
}

// `require` is a function call, so the path is the string argument, and the
// alias is the local the module is bound to.
func TestLuaImports(t *testing.T) {
	res := parse(t, "src/app/cart.lua", `local json = require("cjson")
local util = require "app.util"
local Order = require('app.models.order')
require("app.boot")
Shared = require("app.shared")

local function f()
  return require(name)
end
`)
	cases := map[string]string{
		"cjson": "json", "app.util": "util", "app.models.order": "Order", "app.boot": "", "app.shared": "Shared",
	}
	for path, alias := range cases {
		got, ok := importAlias(res, path)
		if !ok {
			t.Errorf("missing import %q; got %v", path, importPaths(res))
		} else if got != alias {
			t.Errorf("import %q has alias %q, want %q", path, got, alias)
		}
	}
	if len(res.Imports) != len(cases) {
		t.Errorf("imports = %v, want exactly %d (a computed require names no module)", importPaths(res), len(cases))
	}
}

// `source` and `.` name a file. A path relative to the script, or opened by one
// of the spellings of the script's own directory, names a file of the
// repository; a path built from any other variable, an absolute one and a home
// directory name none.
func TestBashImports(t *testing.T) {
	res := parse(t, "scripts/deploy.sh", `source ./lib/log.sh
. lib/net.sh
source "$(dirname "$0")/common.sh"
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/paths.sh"
. "${BASH_SOURCE%/*}/env.bash"
source "$SCRIPT_DIR/lib/retry.sh"
source "${SCRIPT_DIR}"/lib/quote.sh
source ../shared/util.sh

source "$HOME/.profile"
source "$LIB_DIR/x.sh"
source /etc/profile
source ~/.bashrc
source "$1"
echo source other.sh
`)
	for _, want := range []string{
		"scripts/lib/log", "lib/log", "scripts/lib/net", "lib/net", "scripts/common", "scripts/paths",
		"scripts/env", "scripts/lib/retry", "scripts/lib/quote", "shared/util",
	} {
		if !hasImport(res, want) {
			t.Errorf("missing import %q; got %v", want, importPaths(res))
		}
	}
	// A path from the script's directory is not also read as the working
	// directory's.
	for _, bad := range []string{"common", "paths", "env", "lib/retry", "lib/quote"} {
		if hasImport(res, bad) {
			t.Errorf("import %q is relative to the script and was also recorded as written: %v", bad, importPaths(res))
		}
	}
	if len(res.Imports) != 10 {
		t.Errorf("imports = %v, want 10 (a variable, absolute or home path names no file)", importPaths(res))
	}
}

// bats's `load` names a file beside the test, and only in a .bats file: in any
// other it is a command of the script's own.
func TestBashLoadIsBats(t *testing.T) {
	bats := parseAs(t, "bash", "test/deploy.bats", "load helper\nload '../scripts/lib/net'\nsource \"$BATS_TEST_DIRNAME/../deploy.sh\"\n")
	for _, want := range []string{"test/helper", "scripts/lib/net", "deploy"} {
		if !hasImport(bats, want) {
			t.Errorf("missing import %q; got %v", want, importPaths(bats))
		}
	}
	if len(bats.Imports) != 3 {
		t.Errorf("imports = %v, want 3", importPaths(bats))
	}
	plain := parse(t, "scripts/run.sh", "load helper\n")
	if len(plain.Imports) != 0 {
		t.Errorf("`load` outside a .bats file is no import: %v", importPaths(plain))
	}
}

// `source` names a file, relative to the script and as written; `library`
// names a package; a package that ships with R and a path built at run time
// name nothing.
func TestRImports(t *testing.T) {
	res := parse(t, "analysis/run.R", `source("helpers.R")
source('R/util.R', local = TRUE)
source(file.path("R", "paths.R"))
sys.source(here::here("R", "io.R"), envir = env)
library(dplyr)
library("jsonlite")
require(testthat)
requireNamespace("cli", quietly = TRUE)
library(stats)
library(utils)
source(paste0(dir, "/x.R"))
source("/abs/y.R")
source("~/z.R")
library(pkg, character.only = TRUE)
`)
	for _, want := range []string{
		"analysis/helpers", "helpers", "analysis/R/util", "R/util", "analysis/R/paths", "R/paths",
		"analysis/R/io", "R/io", "dplyr", "jsonlite", "testthat", "cli", "pkg",
	} {
		if !hasImport(res, want) {
			t.Errorf("missing import %q; got %v", want, importPaths(res))
		}
	}
	if len(res.Imports) != 13 {
		t.Errorf("imports = %v, want 13 (a base package, a computed path and an absolute one name no file)", importPaths(res))
	}
	for _, i := range res.Imports {
		if i.Alias != "" {
			t.Errorf("import %q kept alias %q", i.Path, i.Alias)
		}
	}
}

// Every Elixir directive is a row that names a module, with the directive in
// the alias, and the functions of a module are qualified by it.
func TestElixirImports(t *testing.T) {
	res := parse(t, "lib/shop/cart.ex", `defmodule Shop.Cart do
  alias Shop.Money
  alias Shop.Billing.Invoice, as: Inv
  alias Shop.{Tax, Billing.Order}
  import Shop.Util
  import Shop.Fmt, only: [fmt: 1, pad: 2]
  require Logger
  require Shop.Mod, as: M
  use Shop.Worker, retries: 3
  import :lists

  def total(c), do: c

  defmodule Line do
    def price(l), do: l
  end
end
`)
	want := map[string]string{
		"Shop.Money": "alias:Money", "Shop.Billing.Invoice": "alias:Inv", "Shop.Tax": "alias:Tax",
		"Shop.Billing.Order": "alias:Order", "Shop.Util": "import", "Shop.Fmt": "import:fmt,pad",
		"Shop.Mod": "alias:M", "Shop.Worker": "use",
	}
	for path, alias := range want {
		got, ok := importAlias(res, path)
		if !ok {
			t.Errorf("missing import %q; got %v", path, importPaths(res))
		} else if got != alias {
			t.Errorf("import %q has alias %q, want %q", path, got, alias)
		}
	}
	if len(res.Imports) != len(want) {
		t.Errorf("imports = %v, want exactly %d (a plain require and an atom name no module)", importPaths(res), len(want))
	}
	// A function is qualified by its module and a nested module by its parent; a
	// symbol with no qualified name of its own is `path:name`.
	fqns := map[string]bool{}
	for _, s := range res.Symbols {
		fqns[s.Name+" "+s.FQN] = true
	}
	for _, f := range []string{
		"Shop.Cart ", "Line lib/shop/cart.ex:Shop.Cart.Line",
		"total lib/shop/cart.ex:Shop.Cart.total", "price lib/shop/cart.ex:Shop.Cart.Line.price",
	} {
		if !fqns[f] {
			t.Errorf("missing symbol %q; got %v", f, fqns)
		}
	}
}

// A dot-source, Import-Module and `using module` name a file or a module; the
// script's own directory is spelled several ways; a path built from anything
// else names nothing.
func TestPowerShellImports(t *testing.T) {
	res := parse(t, "scripts/deploy.ps1", `. ./lib/util.ps1
. "$PSScriptRoot\lib\log.ps1"
. (Join-Path $PSScriptRoot 'net.ps1')
Import-Module ./M.psm1
Import-Module "$PSScriptRoot\Mod.psm1" -Force
Import-Module -Name Billing -Force
Import-Module Pester
using module ./Types.psm1
using namespace System.IO
. $env:HOME\profile.ps1
. $Config.Path
. C:\tools\x.ps1
& ./run.ps1
`)
	for _, want := range []string{
		"scripts/lib/util", "lib/util", "scripts/lib/log", "scripts/net",
		"scripts/M", "M", "scripts/Mod", "Billing", "Pester", "scripts/Types", "Types",
	} {
		if !hasImport(res, want) {
			t.Errorf("missing import %q; got %v", want, importPaths(res))
		}
	}
	for _, bad := range []string{"lib/log", "net", "Mod"} {
		if hasImport(res, bad) {
			t.Errorf("import %q is relative to the script and was also recorded as written: %v", bad, importPaths(res))
		}
	}
	if len(res.Imports) != 11 {
		t.Errorf("imports = %v, want 11 (a variable, a drive path, `using namespace` and `&` name no file)", importPaths(res))
	}

	pester := parse(t, "tests/Deploy.Tests.ps1", "BeforeAll { . $PSCommandPath.Replace('.Tests.ps1', '.ps1') }\n")
	if !hasImport(pester, "tests/Deploy") || len(pester.Imports) != 1 {
		t.Errorf("a Pester test names the file under test: %v", importPaths(pester))
	}
}

// A Groovy import names a class by its package path, a package with a wildcard, a
// static member, and may rename the class. The renamed one keeps both rows: the
// class, and the alias.
func TestGroovyImports(t *testing.T) {
	res := parse(t, "src/main/groovy/com/acme/shop/Cart.groovy", `package com.acme.shop

import com.acme.money.Money
import com.acme.tax.*
import static com.acme.util.Strings.slug
import com.acme.fmt.Formatter as Fmt
import groovy.json.JsonOutput

class Cart {
    def total() { Money.zero() }
}
`)
	want := []struct{ path, alias string }{
		{"com.acme.money.Money", ""}, {"com.acme.tax", ""}, {"com.acme.util.Strings.slug", ""},
		{"com.acme.fmt.Formatter", ""}, {"com.acme.fmt.Formatter", "Fmt"}, {"groovy.json.JsonOutput", ""},
	}
	if len(res.Imports) != len(want) {
		t.Fatalf("imports = %v, want %d rows", res.Imports, len(want))
	}
	for _, w := range want {
		found := false
		for _, i := range res.Imports {
			found = found || (i.Path == w.path && i.Alias == w.alias)
		}
		if !found {
			t.Errorf("missing import %q alias %q; got %v", w.path, w.alias, res.Imports)
		}
	}
}

// A method written with modifiers and no return type is read by the grammar as a
// call, and is a declaration: it must not call itself from the line that
// declares it. `new` is a construction, recorded on the type's name.
func TestGroovyCallShapes(t *testing.T) {
	refs, res := shapeOf(t, "src/main/groovy/a/Cart.groovy", `package a

class Cart {
    def total(items) {
        helper(items)
        this.helper(items)
        Money.zero()
        def t = new TaxCalc()
        t.rate(1)
        x?.go()
    }

    private helper(x) { x }
}
`)
	for _, r := range res.References {
		if r.Name == "helper" && r.Range.Start.Line == 12 {
			t.Errorf("the declaration of helper on line 12 was recorded as a call: %+v", r)
		}
	}
	for name, want := range map[string]struct{ kind, recv string }{
		"zero": {model.ReceiverName, "Money"},
		"go":   {model.ReceiverName, "x"},
	} {
		got, ok := refs[name]
		if !ok || got.ReceiverKind != want.kind || got.Receiver != want.recv {
			t.Errorf("%s = %+v, want %s %q", name, got, want.kind, want.recv)
		}
	}
	if got := refs["TaxCalc"]; got.Kind != "reference.call.new" {
		t.Errorf("new TaxCalc() = %+v, want a construction", got)
	}
}

// Erlang: only `-import` names functions; `m:f()` needs no import. The arity is
// written, and part of what is named.
func TestErlangImports(t *testing.T) {
	res := parse(t, "src/shop_cart.erl", `-module(shop_cart).
-export([total/1]).
-import(shop_util, [slug/1, pad/2]).
-import('odd-mod', [f/0]).
-include("shop.hrl").
-behaviour(gen_server).

total(X) -> shop_util:slug(X).
`)
	want := map[string]string{"shop_util": "import:slug/1,pad/2", "odd-mod": "import:f/0"}
	for path, alias := range want {
		got, ok := importAlias(res, path)
		if !ok {
			t.Errorf("missing import %q; got %v", path, importPaths(res))
		} else if got != alias {
			t.Errorf("import %q has alias %q, want %q", path, got, alias)
		}
	}
	if len(res.Imports) != len(want) {
		t.Errorf("imports = %v, want exactly %d (an include and a behaviour name no function)", importPaths(res), len(want))
	}
}

// Erlang calls: a remote call names its module, `?MODULE` is the module the call
// is in, a variable module names nothing; the arity of a call and of a function
// is counted; a call inside a -spec is a type.
func TestErlangCallShapes(t *testing.T) {
	res := parse(t, "src/m.erl", `-module(m).
-spec f(integer()) -> ok.
f(X) ->
    money:zero(),
    ?MODULE:g(X, 1),
    Mod:h(X),
    g(X, 2).
g(_, _) -> ok.
`)
	type shape struct {
		name, recv, kind string
		args             int
	}
	var got []shape
	for _, r := range res.References {
		n := -1
		if r.ArgCount.Known() {
			n = r.ArgCount.Count()
		}
		got = append(got, shape{r.Name, r.Receiver, r.ReceiverKind, n})
	}
	want := []shape{
		{"zero", "money", model.ReceiverName, 0},
		{"g", "?MODULE", model.ReceiverSelf, 2},
		{"h", model.OpaqueReceiver, model.ReceiverExpr, 1},
		{"g", "", model.ReceiverBare, 2},
	}
	if len(got) != len(want) {
		t.Fatalf("references = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("reference %d = %v, want %v", i, got[i], want[i])
		}
	}
	for _, s := range res.Symbols {
		if s.Name == "g" && (!s.ParamCount.Known() || s.ParamCount.Count() != 2) {
			t.Errorf("g/2 has ParamCount %v", s.ParamCount)
		}
	}
}

// Clojure: one row per namespace a form loads, each saying what the directive
// does with it. `:import` and `:refer-clojure` name no namespace.
func TestClojureImports(t *testing.T) {
	res := parse(t, "src/shop/cart.clj", `(ns shop.cart
  (:require [shop.money :as money :refer [zero]]
            [shop.util :refer :all]
            [shop.tax :refer [rate apply-tax]]
            [shop.data
             [db :as db]
             [cache]]
            shop.plain)
  (:use [shop.fmt :only [fmt]]
        shop.all)
  (:refer-clojure :exclude [first])
  (:import (java.util Date)))

(require '[shop.late :as late])
(defn total [x] x)
`)
	want := map[string]string{
		"shop.money": "alias:money;import:zero", "shop.util": "import", "shop.tax": "import:rate,apply-tax",
		"shop.data.db": "alias:db", "shop.data.cache": "", "shop.plain": "",
		"shop.fmt": "import:fmt", "shop.all": "import", "shop.late": "alias:late",
	}
	for path, alias := range want {
		got, ok := importAlias(res, path)
		if !ok {
			t.Errorf("missing import %q; got %v", path, importPaths(res))
		} else if got != alias {
			t.Errorf("import %q has alias %q, want %q", path, got, alias)
		}
	}
	if len(res.Imports) != len(want) {
		t.Errorf("imports = %v, want exactly %d", importPaths(res), len(want))
	}
}

// Clojure calls: `(f x)` is bare, `(m/f x)` is qualified by an alias or a
// namespace, a special form and a Java method are not calls of the repository.
func TestClojureCallShapes(t *testing.T) {
	refs, res := shapeOf(t, "src/shop/cart.clj", `(ns shop.cart)

(defn total [items]
  (let [s (money/zero)]
    (when-let [x (slugify s)] x)
    (shop.tax/rate s)
    (.getTime (Date.))
    (Math/abs 1)
    '(not-a-call 1)
    #(helper %)))
`)
	for name, want := range map[string]struct{ kind, recv string }{
		"zero":    {model.ReceiverName, "money"},
		"slugify": {model.ReceiverBare, ""},
		"rate":    {model.ReceiverPath, "shop.tax"},
		"abs":     {model.ReceiverName, "Math"},
		"helper":  {model.ReceiverBare, ""},
	} {
		got, ok := refs[name]
		if !ok || got.ReceiverKind != want.kind || got.Receiver != want.recv {
			t.Errorf("%s = %+v, want %s %q", name, got, want.kind, want.recv)
		}
	}
	for _, r := range res.References {
		switch r.Name {
		case "let", "when-let", "defn", "getTime", "Date.", "not-a-call":
			t.Errorf("%q was recorded as a call", r.Name)
		}
	}
}

// Julia: a module by name, a list of names, an alias, and an include rooted at the
// repository. A leading dot says "a module of this one" and is dropped.
func TestJuliaImports(t *testing.T) {
	res := parse(t, "src/Shop.jl", `module Shop

using LinearAlgebra
using .Money, Base.Fmt
using Stats: mean, median
import .Tax: rate, apply as ap
import Geo
import Geo.Shapes as Sh

include("cart.jl")
include(joinpath("util", "format.jl"))
include(path)

end
`)
	want := map[string]string{
		"LinearAlgebra": "import", "Money": "import", "Base.Fmt": "import", "Stats": "import:mean,median",
		"Tax": "import:rate", "Geo": "", "Geo.Shapes": "alias:Sh",
		"src/cart.jl": "include", "src/util/format.jl": "include",
	}
	for path, alias := range want {
		got, ok := importAlias(res, path)
		if !ok {
			t.Errorf("missing import %q; got %v", path, importPaths(res))
		} else if got != alias {
			t.Errorf("import %q has alias %q, want %q", path, got, alias)
		}
	}
	if len(res.Imports) != len(want) {
		t.Errorf("imports = %v, want exactly %d (a computed include names no file)", importPaths(res), len(want))
	}
}

// Julia calls: a short-form definition and a signature are not calls; a
// definition is qualified by the modules around it.
func TestJuliaCallShapes(t *testing.T) {
	refs, res := shapeOf(t, "src/Shop.jl", `module Shop

helper(x) = x * 2
fmt(x::Int)::String = string(x)
conv(x::T) where T = x

function total(items)
    s = Money.zero_money()
    helper(s)
    A.B.deep(s)
    Dict{String,Int}()
    if helper(s) == 3
    end
    s
end

module Inner
    deep(x) = x
end

end
`)
	for _, r := range res.References {
		if r.Name == "conv" || (r.Name == "fmt" && r.Range.Start.Line == 4) || (r.Name == "helper" && r.Range.Start.Line == 3) {
			t.Errorf("the definition of %s on line %d was recorded as a call", r.Name, r.Range.Start.Line)
		}
	}
	for name, want := range map[string]struct{ kind, recv string }{
		"zero_money": {model.ReceiverName, "Money"},
		"deep":       {model.ReceiverPath, "A.B"},
		"Dict":       {model.ReceiverBare, ""},
	} {
		got, ok := refs[name]
		if !ok || got.ReceiverKind != want.kind || got.Receiver != want.recv {
			t.Errorf("%s = %+v, want %s %q", name, got, want.kind, want.recv)
		}
	}
	fqns := map[string]bool{}
	for _, s := range res.Symbols {
		fqns[s.FQN] = true
	}
	for _, f := range []string{"src/Shop.jl:Shop.total", "src/Shop.jl:Shop.Inner", "src/Shop.jl:Shop.Inner.deep", "src/Shop.jl:Shop.helper"} {
		if !fqns[f] {
			t.Errorf("missing symbol %q; got %v", f, fqns)
		}
	}
}

// A single-file component's template renders the components its script
// imported, and only those: a tag nothing imports is a global registration, a
// framework component or an HTML element.
func TestSFCTemplateComponents(t *testing.T) {
	vue := `<script setup lang="ts">
import Badge from './Badge.vue'
import Pill from './Chip.vue'
import PriceTag from './PriceTag.vue'
import { fmt } from '../lib/fmt'
</script>

<template>
  <Badge />
  <Pill :n="1" />
  <price-tag />
  <RouterLink to="/" />
  <button>fmt</button>
  <!-- <PriceTag /> in a comment -->
</template>
`
	res := parse(t, "src/CartView.vue", vue)
	var got []string
	for _, r := range res.References {
		if r.Kind == "reference.call" && r.Range.Start.Line >= 8 {
			got = append(got, fmt.Sprintf("%d %s", r.Range.Start.Line, r.Name))
		}
	}
	want := []string{"9 Badge", "10 Chip", "11 PriceTag"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("template references: got %q, want %q", got, want)
	}
	// The renamed default import is also stated under the component's own name.
	if !hasImportAlias(res, "./Chip.vue", ".Chip") {
		t.Errorf("no import of ./Chip.vue as .Chip: %+v", res.Imports)
	}

	svelte := `<script>
  import Icon from '$lib/Icon.svelte'
</script>

<Icon />
<Missing />
<div>{#if x}<Icon />{/if}</div>
`
	res = parse(t, "src/routes/Page.svelte", svelte)
	got = nil
	for _, r := range res.References {
		if r.Range.Start.Line >= 5 {
			got = append(got, fmt.Sprintf("%d %s", r.Range.Start.Line, r.Name))
		}
	}
	if strings.Join(got, ",") != "5 Icon" {
		t.Errorf("svelte template references: got %q, want [5 Icon]", got)
	}
	if !hasImport(res, "$lib/Icon.svelte") {
		t.Errorf("svelte import not recorded: %+v", res.Imports)
	}
}

func hasImportAlias(res *Result, path, alias string) bool {
	for _, i := range res.Imports {
		if i.Path == path && i.Alias == alias {
			return true
		}
	}
	return false
}

// Solidity names a file by the string that ends an import directive, in four
// forms. The path is rewritten to the file's repository path without the
// extension, and the names a directive binds are rows of their own.
func TestSolidityImports(t *testing.T) {
	src := `pragma solidity ^0.8.0;

import "./A.sol";
import {B, C as D} from "../lib/x.sol";
import * as L from "./L.sol";
import "./Lib.sol" as Lib;
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import "forge-std/Test.sol";

contract T {}
`
	res := parse(t, "src/core/T.sol", src)
	got := map[string]bool{}
	for _, i := range res.Imports {
		got[i.Path+" "+i.Alias] = true
	}
	for _, want := range []string{
		"src/core/A ",
		"src/lib/x ",
		"src/lib/x .B",
		"src/lib/x .D=C",
		"src/core/L ",
		"src/core/L L",
		"src/core/Lib Lib",
		"@openzeppelin/contracts/token/ERC20/IERC20 .IERC20",
		"forge-std/Test ",
	} {
		if !got[want] {
			t.Errorf("missing import row %q in %+v", want, res.Imports)
		}
	}
	// A path that climbs out of the repository names no file in it.
	res = parse(t, "T.sol", "import \"../outside.sol\";\ncontract T {}\n")
	if len(res.Imports) != 0 {
		t.Errorf("an import above the root was kept: %+v", res.Imports)
	}
}

// What a Solidity file calls, constructs and inherits is a reference, and a
// function in a contract is a member of it.
func TestSolidityReferences(t *testing.T) {
	src := `contract T is Base, IFoo {
    using Math for uint;
    Other private other;
    constructor() Base() {}
    function go(Vault v) public onlyOwner {
        Math.max(1, 2);
        this.go(v);
        super.go(v);
        helper();
        new Vault();
        emit Sent(msg.sender);
        require(true);
        abi.encode(1);
    }
    function helper() internal {}
}
`
	res := parse(t, "src/T.sol", src)
	got := map[string]bool{}
	for _, r := range res.References {
		got[r.Kind+" "+r.Receiver+"."+r.Name] = true
	}
	for _, want := range []string{
		"reference.extends .Base", "reference.extends .IFoo", "reference.type .Math",
		"reference.type .Other", "reference.type .Vault", "reference.call .Base",
		"reference.call .onlyOwner", "reference.call Math.max", "reference.call this.go",
		"reference.call super.go", "reference.call .helper", "reference.call.new .Vault",
		"reference.call .Sent", "reference.call .require", "reference.call abi.encode",
	} {
		if !got[want] {
			t.Errorf("missing reference %q in %v", want, got)
		}
	}
	var fqns []string
	for _, s := range res.Symbols {
		fqns = append(fqns, s.FQN)
	}
	for _, want := range []string{"src/T.sol:T.go", "src/T.sol:T.helper"} {
		found := false
		for _, f := range fqns {
			found = found || f == want
		}
		if !found {
			t.Errorf("missing member %q in %q", want, fqns)
		}
	}
}

// Apex has no imports and no packages: a class is global by name. The import
// query exists to say the language's imports are read, and binds nothing; what
// a file constructs and extends is a reference.
func TestApexNoImportsAndConstruction(t *testing.T) {
	if ImportQuery("apex") == "" {
		t.Fatal("no import query for apex")
	}
	src := `public class Foo extends Base implements IFoo, IBar {
    private Bar b;
    public void run(Baz z) {
        Bar x = new Bar();
        new Map<String, Qux>();
        x.go(1);
        Util.helper('a');
    }
}
`
	res := parse(t, "classes/Foo.cls", src)
	if len(res.Imports) != 0 {
		t.Errorf("apex has no imports, got %+v", res.Imports)
	}
	got := map[string]bool{}
	for _, r := range res.References {
		got[r.Kind+" "+r.Receiver+"."+r.Name] = true
	}
	for _, want := range []string{
		"reference.extends .Base", "reference.implementation .IFoo", "reference.implementation .IBar",
		"reference.type .Bar", "reference.type .Baz", "reference.type .Qux",
		"reference.call.new .Bar", "reference.call.new .Map", "reference.call Bar.go", "reference.call Util.helper",
	} {
		if !got[want] {
			t.Errorf("missing reference %q in %v", want, got)
		}
	}
}

// importRows lists every import row as "path alias", for a language whose
// directives keep several rows of one path.
func importRows(res *Result) map[string]bool {
	got := map[string]bool{}
	for _, i := range res.Imports {
		got[i.Path+" "+i.Alias] = true
	}
	return got
}

func wantRows(t *testing.T, res *Result, want []string) {
	t.Helper()
	got := importRows(res)
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing import row %q in %+v", w, res.Imports)
		}
	}
	if len(res.Imports) != len(want) {
		t.Errorf("import rows = %d, want exactly %d: %+v", len(res.Imports), len(want), res.Imports)
	}
}

// Haskell: a module by its dotted name, and what the statement does with it. A
// list names the functions it brings in (the lower-case entries, and a class's
// methods under it), hiding names those it leaves out, and a qualified import
// brings in no bare name.
func TestHaskellImports(t *testing.T) {
	res := parse(t, "src/Shop/Cart.hs", `module Shop.Cart (total) where

import Data.List (sortBy, (\\))
import qualified Data.Map.Strict as M
import Shop.Util (slug, pad, Shape(area, perimeter), Config(..))
import Shop.Tax hiding (apply)
import Shop.Types
import Shop.Money as Money
import Shop.Fmt qualified as Fmt
import {-# SOURCE #-} Shop.Loop
import safe "pkg" Shop.Pkg (go)

total :: Int
total = 0
`)
	wantRows(t, res, []string{
		"Data.List import:sortBy",
		"Data.Map.Strict alias:M",
		"Shop.Util import:slug,pad,area,perimeter",
		"Shop.Tax hiding:apply",
		"Shop.Types import",
		"Shop.Money import;alias:Money",
		"Shop.Fmt alias:Fmt",
		"Shop.Loop import",
		"Shop.Pkg import:go",
	})
}

// Haskell calls: the function of an application, a qualified name wherever it is
// written, a function beside an operator that applies or composes, and one in
// backquotes. A name passed as an argument is not a call, and neither is a
// constructor.
func TestHaskellCallShapes(t *testing.T) {
	refs, _ := shapeOf(t, "src/Shop/Cart.hs", `module Shop.Cart where

f xs = Money.add (sum xs) (slug xs)
g = map (Shop.Util.pad 3) . filter keep $ run 1
h = fmap parse <$> load
k a b = a `+"`plus`"+` b
m = Just total
`)
	for name, want := range map[string]struct{ kind, recv string }{
		"add":    {model.ReceiverName, "Money"},
		"slug":   {model.ReceiverBare, ""},
		"pad":    {model.ReceiverPath, "Shop.Util"},
		"filter": {model.ReceiverBare, ""},
		"plus":   {model.ReceiverBare, ""},
	} {
		got, ok := refs[name]
		if !ok || got.ReceiverKind != want.kind || got.Receiver != want.recv {
			t.Errorf("%s = %+v, want %s %q", name, got, want.kind, want.recv)
		}
	}
	for _, name := range []string{"keep", "total", "Just"} {
		if _, ok := refs[name]; ok {
			t.Errorf("%q is passed or constructed, not called, and was recorded as a call", name)
		}
	}
}

// A name a function binds is not a call of the module's function: a parameter, a
// lambda's argument, a case arm, a do binding and a where. The function that
// calls itself is a call.
func TestShadowedCallsAreIndirect(t *testing.T) {
	cases := []struct{ file, src string }{
		{"src/A.hs", "module A where\nf slug = slug 3\n"},
		{"src/A.hs", "module A where\nf = \\slug -> slug 3\n"},
		{"src/A.hs", "module A where\nf m = case m of\n  Just slug -> slug 3\n  Nothing -> 0\n"},
		{"src/A.hs", "module A where\nf = do\n  slug <- load\n  slug 3\n"},
		{"src/A.hs", "module A where\nf = slug 3\n  where slug = id\n"},
		{"src/a.ml", "let f slug = slug 3\n"},
		{"src/a.ml", "let f = fun slug -> slug 3\n"},
		{"src/a.ml", "let f m = match m with Some slug -> slug 3 | None -> 0\n"},
		{"src/a.ml", "let f x = let slug y = y in slug x\n"},
		{"src/A.fsx", "module A\nlet f slug = slug 3\n"},
		{"src/A.fsx", "module A\nlet f = fun slug -> slug 3\n"},
		{"src/A.fsx", "module A\nlet f m =\n    match m with\n    | Some slug -> slug 3\n    | None -> 0\n"},
		{"src/A.fsx", "module A\nlet f x =\n    let slug y = y\n    slug x\n"},
	}
	for _, c := range cases {
		res := parse(t, c.file, c.src)
		found := false
		for _, r := range res.References {
			if r.Name == "slug" {
				found = true
				if !strings.HasSuffix(r.Kind, IndirectCallSuffix) {
					t.Errorf("%s: slug is a bound name and was recorded as a call (%s):\n%s", c.file, r.Kind, c.src)
				}
			}
		}
		if !found {
			t.Errorf("%s: no reference to slug:\n%s", c.file, c.src)
		}
	}
	for _, c := range []struct{ file, src string }{
		{"src/A.hs", "module A where\nself n = self (n - 1)\n"},
		{"src/a.ml", "let rec self n = self (n - 1)\n"},
		{"src/A.fsx", "module A\nlet rec self n = self (n - 1)\n"},
	} {
		for _, r := range parse(t, c.file, c.src).References {
			if r.Name == "self" && strings.HasSuffix(r.Kind, IndirectCallSuffix) {
				t.Errorf("%s: a function calls itself, and it was marked a bound name:\n%s", c.file, c.src)
			}
		}
	}
}

// OCaml: `open` and `include` bring a module's names in, `module X = A.B`
// renames it, and a local open names a module for one expression and is not an
// import. Each definition is qualified by the struct modules around it.
func TestOCamlImports(t *testing.T) {
	res := parse(t, "src/cart.ml", `open Util
open! Shop.Fmt
include Extra
module M = Money
module T = Shop.Tax
module Inner = struct
  let deep x = x
end
module F (X : S) = struct
  let g x = x
end

let a = let open Money in zero ()
let b = Money.(add 1 2)
`)
	wantRows(t, res, []string{
		"Util import", "Shop.Fmt import", "Extra import", "Money alias:M", "Shop.Tax alias:T",
	})
	for _, s := range res.Symbols {
		if s.Name == "deep" && !strings.HasSuffix(s.FQN, "cart.ml:Inner.deep") {
			t.Errorf("deep = %q, want it qualified by Inner", s.FQN)
		}
		if s.Name == "g" && !strings.HasSuffix(s.FQN, "cart.ml:F.g") {
			t.Errorf("g = %q, want it qualified by F", s.FQN)
		}
	}
}

// OCaml calls: the function of an application, however qualified, and the right
// of `|>`. A name passed as an argument is not a call.
func TestOCamlCallShapes(t *testing.T) {
	refs, _ := shapeOf(t, "src/cart.ml", `let f xs =
  let z = Money.zero () in
  List.iter print_item xs;
  Shop.Tax.rate z + helper z;
  xs |> List.map pad |> sum
`)
	for name, want := range map[string]struct{ kind, recv string }{
		"zero":   {model.ReceiverName, "Money"},
		"iter":   {model.ReceiverName, "List"},
		"rate":   {model.ReceiverPath, "Shop.Tax"},
		"helper": {model.ReceiverBare, ""},
		"sum":    {model.ReceiverBare, ""},
	} {
		got, ok := refs[name]
		if !ok || got.ReceiverKind != want.kind || got.Receiver != want.recv {
			t.Errorf("%s = %+v, want %s %q", name, got, want.kind, want.recv)
		}
	}
	if _, ok := refs["print_item"]; ok {
		t.Errorf("print_item is passed, not called, and was recorded as a call")
	}
}

// F#: `open` brings a module's names in and `module X = A.B` renames it. A
// definition is qualified by the namespaces and modules around it, whichever way
// the file declares them, and a member of a type keeps the type's own name.
func TestFSharpImports(t *testing.T) {
	res := parse(t, "src/Cart.fsx", `namespace Shop

open System
open Shop.Util
open type Shop.Types.Kind
module M = Shop.Money

module Cart =
    let total x = x

    module Inner =
        let deep x = x

type Order() =
    member this.Run() = 1
`)
	wantRows(t, res, []string{"System import", "Shop.Util import", "Shop.Money alias:M"})
	quals := map[string]string{}
	for _, s := range res.Symbols {
		quals[s.Name] = strings.TrimPrefix(s.FQN, "src/Cart.fsx:")
	}
	for name, want := range map[string]string{
		"total": "Shop.Cart.total", "deep": "Shop.Cart.Inner.deep", "Inner": "Shop.Cart.Inner", "Order": "Shop.Order",
	} {
		if quals[name] != want {
			t.Errorf("%s = %q, want %q", name, quals[name], want)
		}
	}

	res = parse(t, "src/Money.fsx", "module Shop.Money\n\nlet zero () = 0\n")
	for _, s := range res.Symbols {
		if s.Name == "zero" && s.FQN != "src/Money.fsx:Shop.Money.zero" {
			t.Errorf("zero = %q, want it qualified by the file's module", s.FQN)
		}
	}
}

// F# calls: the function of an application, however qualified; the grammar reads
// `f a + g b` as `(f a + g) b` and the call of g is still found; and the right of
// `|>`.
func TestFSharpCallShapes(t *testing.T) {
	refs, _ := shapeOf(t, "src/Cart.fsx", `module Cart

let f xs =
    let z = Money.zero ()
    let t = Shop.Tax.rate z + helper z
    xs |> List.map pad |> ignore
    xs |> finish
`)
	for name, want := range map[string]struct{ kind, recv string }{
		"zero":   {model.ReceiverName, "Money"},
		"rate":   {model.ReceiverPath, "Shop.Tax"},
		"helper": {model.ReceiverBare, ""},
		"map":    {model.ReceiverName, "List"},
		"finish": {model.ReceiverBare, ""},
	} {
		got, ok := refs[name]
		if !ok || got.ReceiverKind != want.kind || got.Receiver != want.recv {
			t.Errorf("%s = %+v, want %s %q", name, got, want.kind, want.recv)
		}
	}
}

// Perl: a package by its name, what a statement imports from it, and the package
// a `use parent` inherits from. A pragma names no package.
func TestPerlImports(t *testing.T) {
	res := parse(t, "lib/Shop/Cart.pm", `package Shop::Cart;
use strict;
use warnings;
use constant DEBUG => 0;
use lib 'lib';
use parent -norequire, 'Shop::Base', "Shop::Mixin";
use base qw(Shop::Legacy);
use Shop::Money;
use Shop::Util qw(slug pad);
use Shop::Fmt ('fmt', 'indent');
use Shop::Tax ();
use Shop::Flags qw(:all);
use List::Util 1.45 qw(sum);
require Shop::Extra;
require "helpers.pl";

sub total { 1 }
`)
	wantRows(t, res, []string{
		"Shop::Base parent", "Shop::Mixin parent", "Shop::Legacy parent",
		"Shop::Money import", "Shop::Util import:slug,pad", "Shop::Fmt import:fmt,indent",
		"Shop::Tax ", "Shop::Flags import", "List::Util import:sum", "Shop::Extra ",
	})
}

// Perl calls: a function by its package, a method by a class name, `$self`,
// `$class` and `__PACKAGE__` by the package the call is written in, and
// `SUPER::` by the one inherited from. Any other variable is a value and names
// no package. The sigil of `&f()` is not part of the name.
func TestPerlCallShapes(t *testing.T) {
	refs, res := shapeOf(t, "lib/Shop/Cart.pm", `package Shop::Cart;

sub total {
    my ($self, $m) = @_;
    Shop::Money::zero();
    Shop::Tax->rate(1);
    $self->checkout();
    __PACKAGE__->helper();
    $class->SUPER::build();
    $m->fmt();
    slug($m);
    &pad(1);
    main::go();
}
`)
	for name, want := range map[string]struct{ kind, recv string }{
		"zero":     {model.ReceiverScope, "Shop::Money"},
		"rate":     {model.ReceiverScope, "Shop::Tax"},
		"checkout": {model.ReceiverSelf, "$self"},
		"helper":   {model.ReceiverSelf, "__PACKAGE__"},
		"build":    {model.ReceiverSuper, "SUPER"},
		"fmt":      {model.ReceiverValue, "$m"},
		"slug":     {model.ReceiverBare, ""},
		"pad":      {model.ReceiverBare, ""},
		"go":       {model.ReceiverScope, "main"},
	} {
		got, ok := refs[name]
		if !ok || got.ReceiverKind != want.kind || got.Receiver != want.recv {
			t.Errorf("%s = %+v, want %s %q", name, got, want.kind, want.recv)
		}
	}
	for _, s := range res.Symbols {
		if s.Name == "total" && s.FQN != "lib/Shop/Cart.pm:Shop::Cart.total" {
			t.Errorf("total = %q, want it qualified by its package", s.FQN)
		}
	}

	// A package block and a package statement are both packages; code before
	// either is main's and keeps its name.
	res = parse(t, "bin/run.pl", "sub run { 1 }\npackage Foo { sub a { 1 } }\npackage Bar;\nsub b { 1 }\n")
	owners := map[string]string{}
	for _, s := range res.Symbols {
		owners[s.Name] = s.FQN
	}
	for name, want := range map[string]string{"run": "", "a": "bin/run.pl:Foo.a", "b": "bin/run.pl:Bar.b"} {
		if owners[name] != want {
			t.Errorf("%s = %q, want %q", name, owners[name], want)
		}
	}
}

// Zig: `@import("x.zig")` names a file relative to the importer, bound to a local
// name, to one of its members, or to nothing (`_ = @import`). The standard library
// and the build's packages name no file here.
func TestZigImports(t *testing.T) {
	res := parse(t, "src/app/cart.zig", `const std = @import("std");
const builtin = @import("builtin");
const money = @import("money.zig");
const util = @import("../lib/util.zig");
const Money = @import("money.zig").Money;
pub const add = @import("money.zig").add;
const dep = @import("dep");
comptime {
    _ = @import("tax.zig");
}

pub fn total() void {}
`)
	wantRows(t, res, []string{
		"src/app/money money", "src/lib/util util", "src/app/money .Money", "src/app/money .add", "src/app/tax ",
	})

	// A path that climbs out of the repository names no file in it.
	res = parse(t, "cart.zig", "const o = @import(\"../outside.zig\");\n")
	if len(res.Imports) != 0 {
		t.Errorf("an import above the root was kept: %+v", res.Imports)
	}
}

// Zig calls: a function by name, a member by the value or the file it is called
// on. A builtin is not a call, and a member of a declared type is qualified by it.
func TestZigCallShapes(t *testing.T) {
	refs, res := shapeOf(t, "src/cart.zig", `const std = @import("std");
const money = @import("money.zig");

pub const Cart = struct {
    n: i64,

    pub fn init() Cart {
        return .{ .n = money.zero() };
    }
};

pub fn total(x: i64) i64 {
    std.debug.print("{}", .{x});
    return add(x, @as(i64, 1)) + helper(x);
}
`)
	for name, want := range map[string]struct{ kind, recv string }{
		"zero":  {model.ReceiverName, "money"},
		"print": {model.ReceiverPath, "std.debug"},
		"add":   {model.ReceiverBare, ""},
	} {
		got, ok := refs[name]
		if !ok || got.ReceiverKind != want.kind || got.Receiver != want.recv {
			t.Errorf("%s = %+v, want %s %q", name, got, want.kind, want.recv)
		}
	}
	if _, ok := refs["as"]; ok {
		t.Errorf("@as is a builtin, not a call")
	}
	for _, s := range res.Symbols {
		if s.Name == "init" && s.FQN != "src/cart.zig:Cart.init" {
			t.Errorf("init = %q, want it qualified by Cart", s.FQN)
		}
	}
}
