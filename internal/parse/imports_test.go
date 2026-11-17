package parse

import (
	"context"
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
