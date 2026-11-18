package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestRResolution indexes an R package, a directory of scripts and the
// package's testthat tests, and checks every call against what R makes it
// reach.
//
// What it is built to catch: a package's `R/` files, which share one namespace
// with no import, against a script directory whose neighbours are in scope only
// when `source()`d; `summarise_all` defined in two scripts where one is
// sourced; `filter`, `print` and `message`, which are base or stats functions
// and share a name with a function a script defines; `stats::sd()` and
// `dplyr::mutate()`, which are another package's; a function called through
// `source(file.path(...))`; an R6 method reached through `self$`; and a test
// whose calls are made in a `test_that` callback outside any definition.
func TestRResolution(t *testing.T) {
	got := langEdges(t, rFixture())
	high := model.ConfidenceHigh
	wants := []want{
		// R/ is one namespace: no import, any file.
		{"R/account.R:deposit", "R/validate.R:validate", high},
		{"R/account.R:deposit", "R/log.R:log_event", high},
		{"R/account.R:balance", "R/account.R:sum_amounts", high},
		{"R/validate.R:validate", "R/validate.R:check_positive", high},
		// R6 members are indexed under their own names, with no class in them. A
		// call on `self` or on a value of unknown type is bound at low, to the
		// one member of that name there is.
		{"R/ledger.R:add", "R/validate.R:validate", high},
		{"R/ledger.R:add", "R/ledger.R:total", model.ConfidenceLow},
		{"R/ledger.R:use_ledger", "R/ledger.R:total", model.ConfidenceLow},
		// A script reaches what it sources, written two ways.
		{"scripts/run.R:main", "scripts/lib/io.R:read_data", high},
		{"scripts/run.R:main", "scripts/lib/io.R:summarise_all", high},
		{"scripts/run.R:main", "scripts/run.R:tidy", high},
		{"scripts/run.R:(top level)", "scripts/run.R:main", high},
		{"scripts/report.R:build_report", "scripts/lib/stats.R:describe_all", high},
		{"scripts/lib/io.R:read_data", "scripts/lib/io.R:parse_header", high},
		// A package's tests call its functions, which testthat attaches.
		{"tests/testthat/test-account.R:(top level)", "R/account.R:deposit", high},
		{"tests/testthat/test-account.R:(top level)", "R/account.R:balance", high},
		// pkg::f() where pkg is this repository's own package (DESCRIPTION's
		// Package: field) is a call into R/.
		{"scripts/run.R:main", "R/validate.R:validate", high},
	}
	forbid := []want{
		// scripts/lib/other.R is not sourced by run.R.
		{Src: "scripts/run.R", Dst: "scripts/lib/other.R"},
		// And a neighbour of run.R that is not sourced is no package.
		{Src: "scripts/run.R", Dst: "scripts/neighbour.R"},
		// filter, print and message are the language's.
		{Src: "scripts/run.R:main", Dst: "filter"},
		{Src: "R/log.R:log_event", Dst: "scripts/"},
		// Decoys for the own-package rule: `ledger::validate` is R/'s, never the
		// script directory's namesake; `otherpkg::validate` is another
		// package's; `ledger::no_such_fn` is nobody's.
		{Src: "scripts/run.R:main", Dst: "scripts/lib/other.R:validate"},
		{Src: "scripts/run.R:main", Dst: "no_such_fn"},
	}
	scoreEdges(t, got, wants, forbid, nil)
}

func rFixture() map[string]string {
	return map[string]string{
		"DESCRIPTION": "Package: ledger\nTitle: A Ledger\nVersion: 0.1.0\nImports: dplyr\n",
		"NAMESPACE":   "export(deposit)\nexport(balance)\n",
		"R/account.R": `sum_amounts <- function(acct) {
  sum(unlist(acct$amounts))
}

deposit <- function(acct, amount) {
  validate(amount)
  log_event("deposit")
  acct$amounts <- c(acct$amounts, amount)
  acct
}

balance <- function(acct) {
  sum_amounts(acct)
}
`,
		"R/validate.R": `check_positive <- function(x) {
  if (x <= 0) stop("must be positive")
  invisible(x)
}

validate <- function(x) {
  stopifnot(is.numeric(x))
  check_positive(x)
}
`,
		"R/log.R": `log_event <- function(msg) {
  message(msg)
  print(msg)
}
`,
		"R/ledger.R": `Ledger <- R6Class("Ledger",
  public = list(
    items = NULL,
    add = function(x) {
      validate(x)
      self$total()
    },
    total = function() {
      sum(self$items)
    }
  )
)

Cache <- R6Class("Cache",
  public = list(
    add = function(k) {
      k
    },
    flush = function() {
      NULL
    }
  )
)

use_ledger <- function(l) {
  l$total()
  l$flush()
}
`,
		"scripts/run.R": `source("scripts/lib/io.R")
source(file.path("scripts", "lib", "stats.R"))
library(dplyr)

tidy <- function(df) {
  df
}

main <- function() {
  data <- read_data("x.csv")
  out <- filter(data, y > 1)
  out <- dplyr::mutate(out, z = 1)
  s <- stats::sd(out$z)
  ledger::validate(s)
  otherpkg::validate(s)
  ledger::no_such_fn(s)
  summarise_all(tidy(out))
}

main()
`,
		"scripts/report.R": `source("lib/stats.R", chdir = TRUE)

build_report <- function(df) {
  describe_all(df)
}
`,
		"scripts/lib/io.R": `parse_header <- function(path) {
  readLines(path, n = 1)
}

read_data <- function(path) {
  parse_header(path)
  read.csv(path)
}

summarise_all <- function(df) {
  summary(df)
}
`,
		"scripts/lib/stats.R": `describe_all <- function(df) {
  summary(df)
}
`,
		"scripts/lib/other.R": `validate <- function(x) {
  x
}

summarise_all <- function(df) {
  df
}

filter <- function(df, cond) {
  df
}

mutate <- function(df, ...) {
  df
}

sd <- function(x) {
  0
}
`,
		"scripts/neighbour.R": `tidy <- function(df) {
  df
}
`,
		"tests/testthat.R": `library(testthat)
library(ledger)
test_check("ledger")
`,
		"tests/testthat/test-account.R": `test_that("a deposit adds to the balance", {
  acct <- deposit(list(amounts = 0), 5)
  expect_equal(balance(acct), 5)
})
`,
	}
}
