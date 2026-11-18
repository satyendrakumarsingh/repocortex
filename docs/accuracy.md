# Resolution accuracy

RepoCortex labels every relationship with a confidence, and until this document
existed those labels were a design intention rather than a measurement. This is
the measurement.

Everything here is reproducible with the tool itself. If you disagree with a
number, run the four commands in "Method" against a repository you know and
send the disagreement.

The newest measurement is [the second verification of 13 November](#2026-11-13-the-second-verification-and-what-is-still-wrong),
near the end of this page, which re-read 14 projects after the fixes and lists what is
still wrong. Before it, ["Receivers"](#receivers-binding-a-call-by-what-it-was-made-on),
below the first result, re-measured every tier by hand on nine projects after the
resolver stopped ignoring what a call was made on; it is where the floor for
`medium` is stated, and it corrects the adjudication the older tables in this
document were computed with. Where a later section and an older one disagree
about what a tier is worth, the later one is the one to read. The dated
sections run in order, and "Reproducing" is last.

## Result

Heuristic `CALLS` edges adjudicated against a compiler-backed index, across
three languages and five repositories — **30,975 adjudicated edges**:

| Confidence | Go | TypeScript | Python | judged |
|---|---|---|---|---|
| `high` | **86.8 – 96.1%** | **87.0%** | **87.1 – 94.3%** | 24,284 |
| `medium` | **5.7 – 18.5%** | **14.6%** | **37.5%** | 6,473 |
| `low` | **2.0 – 2.8%** | **0.0%** | **0.0%** | 218 |

Java was measured later, separately, on a smaller pool: 100.0% `high` on 82 of
82 judged edges, 32.7% recall — see "Java: measured" below.

*Superseded in part, 13 November 2026.* These figures were adjudicated against a
compiler index on five repositories in three languages and stand for them. The
reading drawn from them below ("`high` is worth acting on, in every language
measured. `medium` is not.") does not hold for the languages and projects read
by hand since: see ["What this supersedes above"](#what-this-supersedes-above)
in the second verification.

Per repository:

| Repository | Language | Files | `high` | `medium` | `low` | all tiers | judged |
|---|---|---|---|---|---|---|---|
| prometheus | Go | 1,030 | 86.8% | 5.7% | 2.0% | 65.2% | 16,985 |
| nest | TypeScript | 1,013 | **87.5%** | 17.4% | 0.0% | 75.1% | 1,669 |
| scrapy | Python | 480 | **91.8%** | 44.3% | 0.0% | 80.6% | 8,574 |
| requests | Python | 19 | 94.3% | — | — | 94.3% | 228 |

Python and TypeScript improved after the tier order below was corrected;
Go is unchanged, which was the point of making the order language-dependent
rather than picking one.

### Where the remaining errors come from

The `high` disagreements were bucketed against `scip-python` on scrapy, and
one shape dominated: 45% of them were a reference bound to a same-named symbol
in a NEIGHBOURING file rather than the one the file imported.

    tests/test_addons.py
      9:  from scrapy.utils.test import get_crawler
      58:     crawler = get_crawler(...)        # no local definition

Directory proximity was consulted before the import list, so an unrelated
`get_crawler` in another test file won — at high confidence, 144 times from
that one name. Fixing it moved Python from 87.1% to 91.8%.

The correction is that the ORDER is language-dependent, not that imports
always win:

  - Where a directory IS the package (Go, Java, Kotlin) an unqualified name
    resolves in-package; the imported one would have to be written `pkg.Foo()`.
    Proximity first. Reversing this cost prometheus 0.8 points.
  - Where each file is its own module (Python, TypeScript, JavaScript)
    adjacency implies nothing. Imports first.

A second fix followed from the same measurement: a relative import names a
FILE (`./sibling` → `sibling.ts`), and the import matcher only compared
directory segments, so every relative import missed and proximity answered
instead. Matching file names — only in the languages where an import can name
one — took TypeScript from 87.0% to 87.5%.

Ground truth: `scip-go` 0.2.7, `scip-typescript` 0.3.14, `scip-python` 0.6.6.

### What this says

**`high` is worth acting on, in every language measured. `medium` is not.**

`high` lands between 86.8% and 96.1% across Go, TypeScript and Python — five
repositories, three ecosystems, three different compiler-backed indexers. That
consistency is the useful part: the label means roughly the same thing wherever
you meet it, so a consumer can trust it to rank without knowing which language
produced it.

`medium` ranges from 5.7% to 37.5% and is the tier to distrust. It is also not
rare — 6,473 of the adjudicated edges — which is why the all-tiers figure sits
between 65% and 94% while `high` alone stays near 90%. An agent consuming every
edge equally gets the low number. One filtering with `--min-confidence high`
gets the high one.

`low` is 0–2.8%. It is a hint that a name exists, nothing more.

Two claims this project used to make are corrected by the above. `medium` was
documented as "probable"; at 5.7% on prometheus it is not. `high` was "wrong
only under shadowing or reflection"; at ~87% roughly one edge in eight is wrong
for reasons not yet characterised, and saying so is what makes characterising
them possible.

## Receivers: binding a call by what it was made on

Measured 12 November 2026, on the eight projects and by the method of
[the verification record](verification/2026-11-12/README.md), so the two can be
compared. The record found that the largest source of wrong high-confidence
edges, in every language it checked, was one defect: a call with a receiver or
a qualifier was bound as if it had none. `errors.New(...)` bound to the
package's own `New`; a Java local `reader` was stored as a call and bound to a
method `reader(String)`; `self.inner.foo()` inside `fn foo` bound to itself.

This section is what replaced that, and what it measured.

### The floors

| Tier | Promise | Where it comes from |
|---|---|---|
| `high` | at least 86.8% of edges are right | the floor this document has published since the first measurement |
| `medium` | at least 60% | stated here for the first time; before this section `medium` had no floor and measured 5.7% to 37.5% |
| `low` | none: a hint that a name exists | unchanged |

A rule is given the tier whose floor its own measurement meets, per language
where the rule's precision depends on the language. A rule that measured 47%
is `low`, whatever its evidence sounds like, and a rule that reads like a guess
and measured 96% is `high`.

### What is recorded

Every call reference now carries the shape of its receiver
(`symbol_references.receiver_kind`, migration 0015), in one vocabulary for
every language:

| Kind | Source | `receiver` holds |
|---|---|---|
| `bare` | `f()` | nothing: the source wrote no receiver |
| `name` | `x.f()` | the identifier, where the enclosing code does not declare it: an import, a type, a global |
| `value` | `x.f()` | the identifier, where the enclosing code declares it with no stated type |
| `type` | `x.f()` | the TYPE the source states for `x`: a typed declaration, a typed parameter, a constructor (`x := &T{}`, `new T()`) |
| `path` | `a.b.f()` | the chain, as written |
| `scope` | `a::f()`, `a::b::f()` | the path through the scope operator, which names a module or a type |
| `self` | `self.f()`, `this.f()` | the token |
| `super` | `super.f()`, `super().f()`, `base.f()` | the token |
| `expr` | `g().f()`, `a[i].f()` | `?` |

It is read from the tag's own two ranges and one walk of the syntax tree
(`internal/parse/shape.go`), not from a query per language, so a grammar gains
it without a line written for it. The same walk qualifies every definition by
what encloses it (`Context.Set`, `outer.<locals>.inner`), which is what lets
`self.f()` be bound to the enclosing type's `f`.

A receiver identifier is no longer a call reference. The inferred tags queries
emitted one for every `x` in `x.f()`:

| Project | Call references before | After | Receiver rows gone |
|---|---:|---:|---:|
| gson (Java) | 29,093 | 20,200 | 8,893 (30.6%) |
| ripgrep (Rust) | 18,723 | 16,879 | 1,844 (9.8%) |
| flask (Python) | 4,671 | 3,904 | 767 (16.4%) |
| sinatra (Ruby) | 14,615 | 14,596 | 19 |
| zod (TypeScript) | 53,387 | 53,374 | 13 |
| gin (Go), leveldb (C++) | 9,311 / 6,126 | 9,311 / 6,126 | 0: their queries never tagged the receiver |

redis and Newtonsoft.Json are absent from this table because their reference
totals move for another reason: which files hit the parse timeout differs from
run to run on a loaded machine. Two runs of builds whose parsers are identical
recorded 77,411 and 78,849 call references on redis, and 13,456 and 14,276 on
Newtonsoft.Json. The C and C# tags queries capture the callee alone, so neither
had receiver rows to lose.

### The rules, with what each measured

Each sampled edge was checked by reading the call site and the target. The
samples are stratified: every k-th edge by id within each rule, 12 to 25 per
rule per project, 1,408 edges in all. The reviewers were sub-agents of an AI
assistant, as in the record, judging strictly (the right overload is required;
a standard-library or third-party call bound to a repository symbol is wrong).
"Right / judged" pools every language the rule fired in.

| Rule | Tier | Right / judged | Edges | Notes |
|---|---|---:|---:|---|
| bare call, defined in the same file | high | 143 / 145 | 14,491 | |
| bare call, defined in the enclosing scope | high | 41 / 41 | 347 | a definition local to the calling function |
| bare call, same package directory (Go) | high | 25 / 25 | 887 | medium where a package declares the name twice |
| bare call, imported by name | high | 23 / 24 | 299 | ECMAScript named imports, Python `from x import y` |
| bare call, statically imported (Java) | high | 8 / 8 | 8 | |
| bare or `self`/`this` call, declared on the enclosing type | high | 136 / 138 | 3,994 | |
| inherited: declared on an ancestor | high | 45 / 45 | 257 | `super`, and members the type does not declare; needs recorded inheritance |
| receiver's stated type declares it | high | 107 / 107 | 5,146 | |
| receiver names a type that declares it | high | 65 / 65 | 1,152 | `Type.f()`, `Type::f()` |
| as the two above, type identified only as the one of that name | high in Rust and C#, medium elsewhere | 48 / 50 | 2,184 | C# 31 / 33, Rust 12 / 12, Ruby 5 / 5 |
| qualified by an imported package or module that defines it | high | 37 / 37 | 468 | Go import names, ECMAScript namespace imports resolved to the file |
| qualified by an imported package that re-exports it | high | 24 / 24 | 435 | |
| qualified by the module whose file defines it (Rust) | high | 12 / 12 | 30 | `messages::errored()` and messages.rs |
| receiver named after the type that declares it | high in Go, Java, C++, Rust, Ruby; medium elsewhere | 54 / 55 | 320 | a naming convention, and it measured as more than one |
| Ruby: a method of this name in a required file | high | 24 / 25 | 745 | low where several required files define one |
| Ruby: the only method of this name in the file | high | 24 / 25 | 352 | |
| bare call, free function in the same directory, no import | medium; low in C++; off in Ruby and Python | C 23 / 25, Rust 25 / 25, C++ 6 / 12, Ruby 0 / 12 | 9,208 | |
| unknown receiver, the only member of that name in the repository | high in Go; medium in Rust, Java, C#; low elsewhere; never above low for an `expr` receiver | see below | 3,000 | |
| overloaded, the first declaration | low | 49 / 96 | 4,065 | the type is right; which overload is not chosen |
| unique name in the repository, no import evidence | low | 13 / 15 | 64 | |
| Python: single match in an imported module, receiver's class unknown | medium | 9 / 12 | 114 | |
| Python: defined in the same file, receiver's class unknown | low | 2 / 12 before two exclusions, 1 / 7 after | 41 | was medium |

The unknown-receiver rule is the one whose worth is a fact about the language:

| Language | Right / judged | Tier |
|---|---:|---|
| Go | 25 / 25 by hand; 458 / 458 against `scip-go` on gin while the rule was still labelled `medium`, and 10,396 / 10,398 for the whole `high` tier on this repository with the rule in it | high |
| C# | 25 / 28 | medium |
| Java | 11 / 12 | medium |
| Rust | 22 / 25 | medium |
| Ruby | 18 / 25 | low |
| TypeScript | 7 / 12 | low |
| C++ | 3 / 12 | low |

A Go value's methods are declared by a package and never by the language, so a
name the repository declares once is seldom anyone else's. Everywhere else the
language's own library declares methods on every string and collection, which
is why the rule also refuses a short list of core member names per language
(`internal/resolve/infer/corenames.go`): before that list, zod measured 3 right
of 14 and ripgrep 4 of 8 on this rule.

What no rule does: bind a qualified call to a free function, or to the
function it is written in, because the name matched.

### Calls that leave the repository

A call qualified by an import of the standard library or of a declared
dependency, or made on a type imported from one, binds to nothing and is
recorded as external (`symbol_references.external`). It is a subset of
`unresolved`, set only on evidence: the import table, the repository's
directory layout, and its manifests.

| Project | Unresolved | of which external |
|---|---:|---:|
| gin | 5,048 | 4,003 |
| gson | 10,849 | 6,620 |
| zod | 36,353 | 13,524 |
| Newtonsoft.Json | 4,938 | 999 |
| ripgrep | 9,055 | 948 |
| sinatra | 9,727 | 314 |
| flask | 1,637 | 252 |
| leveldb | 4,037 | 229 |

### Candidates

A call binds only to a function or a method written in a language the caller
can call; the table is `languageFamilies` in
`internal/resolve/infer/calltraits.go`. A field, a constant, a configuration
key, a table column and a dependency line are not candidates. Dependency
bindings are not kept under another kind: they are gone.

| | CALLS edges to something that is not a function or method | CALLS edges into another language |
|---|---|---|
| Before | gin 196, gson 927, flask 330, ripgrep 1,136, leveldb 33, Newtonsoft.Json 39, zod 16, redis 10 | gson 31, redis 11, zod 9, flask 7, ripgrep 3 |
| After | 0 on every project | 0, other than TypeScript to JavaScript and C++ to a header indexed as C |

A call that constructs a type is an `INSTANTIATES` edge to the type, not a
`CALLS` edge: `Account(...)` in Python, `Type.new` in Ruby where the type
writes no `new`, a Go conversion. The choice is the type rather than its
constructor because several of these languages have no constructor in source
and a compiler index reports the type. `callers` and `find_callers` read
`CALLS` only; impact analysis, test discovery, data-flow traversal and
`references` follow both.

### Precision per tier

Thirty judged edges per tier per project, allocated across the tier's rules in
proportion to their populations (fewer where the tier holds fewer judged
edges). "Weighted" is each rule's sampled precision weighted by its population
in that tier.

| Project | `high` sample | weighted | `medium` sample | weighted | `low` sample | weighted |
|---|---:|---:|---:|---:|---:|---:|
| gin (Go) | 30 / 30 | 99.9% | 1 / 1 | — | none judged | — |
| gson (Java) | 30 / 30 | 100.0% | 4 / 4 | — | 19 / 30 | 63.5% |
| zod (TypeScript) | 30 / 30 | 97.1% | none (1 edge) | — | 7 / 12 | 58.3% |
| flask (Python) | 29 / 30 | 99.0% | 10 / 13 | 75.4% | 8 / 16 | 32.9% |
| sinatra (Ruby) | 30 / 30 | 98.2% | 8 / 8 | — | 18 / 28 | 68.3% |
| ripgrep (Rust) | 30 / 30 | 100.0% | 28 / 30 | 93.8% | 18 / 19 | 94.0% |
| leveldb (C++) | 29 / 29 | 100.0% | none (0 edges) | — | 14 / 26 | 56.3% |
| redis (C) | 25 / 25 | 100.0% | 23 / 25 | 92.0% | 3 / 3 | — |
| Newtonsoft.Json (C#) | 27 / 30 | 95.1% | 22 / 24 | 91.7% | 13 / 28 | 58.4% |

The record measured `high` at 54% to 100% and `medium` at 10% to 100% on the
same projects. Every `high` here is above the 86.8% floor and every `medium`
with more than a handful of edges is above 60%.

Sampled edge ids, in the index each project produced at this commit, with the
ones judged wrong after the bar:

- gin high: 1, 45, 47, 86, 211, 266, 278, 313, 375, 577, 631, 664, 802, 818, 900, 969, 1005, 1102, 1260, 1266, 1286, 1291, 1433, 1507, 1619, 1713, 1801, 1828, 1943, 2026 | none. Medium: 118 | none.
- gson high: 120, 160, 205, 468, 532, 720, 966, 1014, 1272, 1547, 1739, 1768, 2087, 2390, 2613, 3019, 3307, 3408, 3689, 3741, 4065, 4417, 5203, 5224, 5474, 5684, 5993, 6141, 6322, 6503 | none. Medium: 291, 507, 2781, 3434 | none. Low: 150, 172, 218, 852, 1133, 1785, 2034, 2157, 2363, 2838, 2993, 3169, 3303, 3515, 3639, 3815, 3992, 4169, 4336, 4489, 4572, 4667, 4782, 4931, 5063, 5157, 5678, 7467, 7804, 8073 | 172, 218, 2034, 2157, 2363, 3169, 3992, 4489, 5678, 7467, 7804.
- zod high: 20, 91, 134, 172, 230, 249, 299, 348, 513, 585, 845, 868, 1031, 1111, 1209, 1312, 1441, 1464, 1606, 1631, 1755, 1831, 1961, 2350, 2421, 2483, 2610, 2760, 2773, 2912 | none. Low: 257, 541, 561, 598, 629, 693, 729, 773, 780, 797, 1104, 2354 | 541, 773, 780, 797, 1104.
- flask high: 21, 22, 23, 26, 30, 60, 81, 85, 88, 97, 140, 156, 170, 178, 257, 275, 303, 409, 429, 443, 501, 530, 533, 614, 650, 688, 709, 721, 746, 833 | 178. Medium: 92, 176, 284, 331, 363, 481, 567, 591, 629, 638, 738, 869, 1000 | 92, 629, 738. Low: 117, 133, 167, 188, 217, 254, 268, 344, 346, 585, 594, 607, 642, 647, 744, 914 | 254, 268, 585, 594, 607, 642, 647, 744.
- sinatra high: 6, 8, 21, 72, 134, 175, 176, 198, 257, 329, 464, 566, 632, 705, 760, 822, 910, 944, 998, 1084, 1127, 1168, 1211, 1251, 1293, 1385, 1456, 1464, 1572, 2067 | none. Medium: 389, 412, 462, 730, 1313, 1316, 1320, 1326 | none. Low: 86, 217, 228, 396, 452, 467, 526, 664, 753, 801, 852, 946, 1003, 1049, 1105, 1184, 1268, 1329, 1365, 1452, 1634, 1716, 1825, 1890, 1978, 2037, 2220, 2245 | 86, 217, 228, 452, 467, 1268, 1634, 1716, 1890, 2220.
- ripgrep high: 27, 61, 65, 76, 92, 175, 805, 825, 982, 1112, 1346, 1417, 1448, 1504, 1619, 1714, 1803, 1872, 1954, 2202, 2217, 2313, 2497, 2647, 2727, 2835, 3015, 3060, 3242, 3321 | none. Medium: 126, 154, 179, 207, 250, 266, 275, 299, 323, 350, 376, 406, 432, 459, 485, 512, 542, 568, 592, 615, 639, 923, 1591, 1682, 1938, 2095, 2519, 2944, 3653, 3852 | 1682, 2095. Low: 83, 107, 354, 878, 1085, 1363, 1476, 2351, 2513, 2528, 2770, 3017, 3178, 3282, 3466, 3774, 4078, 4128, 4172 | 83.
- leveldb high: 1, 4, 100, 147, 207, 246, 283, 319, 355, 391, 427, 466, 509, 548, 584, 620, 663, 704, 742, 754, 821, 877, 1038, 1074, 1110, 1189, 1229, 1287, 1361 | none. Low: 20, 21, 23, 124, 152, 185, 494, 638, 762, 784, 830, 844, 894, 917, 933, 949, 965, 981, 997, 1132, 1165, 1205, 1221, 1325, 1343, 1374 | 21, 494, 638, 784, 917, 933, 949, 965, 981, 997, 1165, 1221.
- redis high: 236, 795, 1325, 2151, 2890, 3624, 4062, 4539, 5125, 5599, 6015, 6460, 7915, 8826, 10066, 10953, 11999, 12885, 13412, 14155, 14588, 15470, 16907, 18031, 19487 | none. Medium: 578, 1715, 2451, 3072, 4645, 6838, 7340, 7767, 8487, 8927, 9403, 9853, 10376, 11021, 11623, 12094, 12691, 13562, 14739, 15286, 16342, 16879, 17419, 17882, 18975 | 8487, 15286. Low: 19938, 19939, 19940 | none.
- Newtonsoft.Json high: 93, 146, 244, 610, 672, 1344, 1660, 1682, 1683, 1699, 1720, 1924, 2190, 2418, 2929, 2933, 3156, 3478, 3697, 3707, 4746, 4861, 4952, 5015, 5182, 5207, 5357, 5446, 5704, 5910 | 1682, 1720, 4746. Medium: 233, 444, 489, 848, 961, 1017, 1061, 1119, 1175, 1362, 1384, 1406, 1521, 1587, 1764, 1872, 2085, 2205, 2446, 2562, 2625, 3015, 3064, 5846 | 1872, 5846. Low: 163, 534, 544, 556, 570, 620, 680, 1582, 1640, 1648, 1703, 1938, 1980, 2784, 3153, 3198, 3316, 3391, 3535, 3702, 3757, 4332, 4688, 4689, 5003, 5683, 6081, 6086 | 534, 544, 556, 570, 620, 1640, 1648, 1703, 1938, 2784, 3153, 3391, 3702, 3757, 6086.

The redis and Newtonsoft.Json ids are from one run; their edge ids shift with
which files time out. The judgements were made on an earlier build of the same
rules and carried to this one by call site and target: every judged edge
survived on six projects, 175 of 182 on flask, 151 of 164 on sinatra (all 13
that did not were wrong edges the last changes removed) and 159 of 199 on
Newtonsoft.Json, where restoring owners to 671 methods rebound many calls.

### Edges and outcomes, before and after

Call references in the language named, and the edges their file produced.

| Project | Resolved | Ambiguous | Unresolved | `high` | `medium` | `low` | `INSTANTIATES` |
|---|---|---|---|---|---|---|---|
| gin | 3,572 → 2,919 | 1,410 → 1,344 | 4,329 → 5,048 | 3,448 → 2,814 | 103 → 50 | 8 → 39 | 0 → 10 |
| gson | 9,201 → 8,122 | 7,850 → 1,229 | 12,042 → 10,849 | 7,199 → 5,610 | 1,851 → 72 | 39 → 2,371 | 0 |
| zod | 6,499 → 6,453 | 23,496 → 10,568 | 23,392 → 36,353 | 1,742 → 2,699 | 674 → 1 | 1 → 74 | 0 |
| flask | 1,109 → 937 | 1,702 → 1,330 | 1,860 → 1,637 | 895 → 540 | 173 → 116 | 26 → 58 | 0 → 217 |
| sinatra | 2,932 → 3,149 | 5,246 → 1,720 | 6,437 → 9,727 | 2,073 → 1,616 | 269 → 10 | 0 → 671 | 0 → 36 |
| ripgrep | 4,906 → 4,355 | 5,825 → 3,469 | 7,992 → 9,055 | 3,034 → 3,030 | 1,419 → 870 | 286 → 397 | 0 → 50 |
| leveldb | 1,997 → 1,596 | 990 → 493 | 3,139 → 4,037 | 1,242 → 965 | 243 → 0 | 111 → 334 | 0 → 34 |
| Newtonsoft.Json | 4,604 → 6,253 | 6,275 → 3,085 | 2,168 → 4,938 | 2,857 → 3,524 | 916 → 669 | 776 → 1,993 | 0 |

Fewer references are resolved on most projects, and that is the point of the
change rather than its cost: of the 3,448 `high` edges gin had, about 170 were
standard-library calls bound to gin symbols and 196 pointed at something that
is not a function. The flask "before" is the scope-aware Python resolver merged
the same day, not the older one the record measured; its call edges included
239 to classes, now `INSTANTIATES`, and the receiver rows.

### Recall

Call-site recall on the five files per project the record used, by its method:
the first ten call sites in each file whose target is defined in the
repository. The sites were listed and checked by hand once, on this build, and
the build before the change was scored on the same sites.

| Project | Sites | Correct before | Correct after | After: wrong / ambiguous / unbound / no enclosing function |
|---|---:|---:|---:|---|
| gin | 37 | 22 (59%), 12 wrong | 33 (89%) | 0 / 4 / 0 / 0 |
| ripgrep | 33 | 17 (52%) | 24 (73%) | 0 / 6 / 3 / 0 |
| zod | 32 | 13 (41%) | 18 (56%) | 0 / 0 / 1 / 13 |
| flask | 31 | 22 (71%), 6 wrong | 24 (77%) | 4 / 3 / 0 / 0 |
| gson | 42 | 22 (52%) | 23 (55%) | 1 / 2 / 16 / 0 |
| sinatra | 31 | 20 (65%), 2 wrong | 20 (65%) | 1 / 0 / 10 / 0 |
| redis | 45 | 39 (87%) | 39 (87%) | 0 / 0 / 6 / 0 |
| Newtonsoft.Json | 43 | 15 (35%) | 15 (35%) | 4 / 0 / 24 / 0 |
| leveldb | 41 | 3 (7%) | 3 (7%) | 0 / 1 / 31 / 6 |

Recall rose or held on every project. What it did not reach is unchanged and is
not about receivers: constructor calls with `new` are not captured in Java, C#
and TypeScript (all 16 unbound gson sites), C++ loses classes declared in
headers and out-of-line members, and a call at module top level has no
enclosing function to hang an edge on (13 of zod's 32 sites).

### Against a compiler index

`scip-go` 0.2.7 and `scip-python` 0.6.6, by `repocortex accuracy`. A Rust index
could not be produced: `rust-analyzer` on this machine is a toolchain proxy
with no component behind it, and installing one was out of bounds.

| Corpus | | `high` | `medium` | all tiers | recall | wrong edges |
|---|---|---|---|---|---|---|
| gin, 2,239 call sites | before | 99.1% (1,983 / 2,002) | 0 / 11 | 98.5% | 88.6% | 30 |
| | after | 100.0% (1,976 / 1,976) | 28 / 28 | 99.9% | 89.6% | 3 |
| this repository, 11,300 call sites | before | 99.0% (10,301 / 10,408) | 54 / 86 | 98.7% | 91.7% | 139 |
| | after | 100.0% (10,396 / 10,398) | 2 / 10 | 99.9% | 92.1% | 10 |
| flask | before, 431 sites | 98.9% (351 / 355) | 23 / 24 | 97.9% | 87.2% | 8 |
| | after, 341 sites | 98.7% (293 / 297) | 17 / 17 | 97.8% | 93.0% | 7 |

These figures understate what changed, and the reason is in the method. A
compiler index holds facts only about symbols the repository defines, so the
calls this change is mostly about — `errors.New` bound to gin's `New` — sit on
lines the compiler has no fact for and are not adjudicated at all. The hand
samples above are where those show.

flask's two rows have different denominators because the adjudication itself
was corrected; see the next heading. The 90 sites that left were receivers.

### A correction to the adjudication

A call site used to be adjudicated when the compiler resolved its line to
exactly one definition, whatever that definition was. For `bp.route("/")`
`scip-python` resolves only `bp`: `route` lives in a library it has no source
for. The parser also emitted the receiver as a "call" to `bp`, the resolver
bound that row to the variable, and the line was scored as a call site answered
correctly at `high`. On flask that was 59 of the 351 sites scored right.

A compiler fact now adjudicates a call only when it names the callee, and a
resolver edge is compared only with a fact about its own name (`accuracyCTE` in
`internal/registry/accuracy.go`, and the same in `doctor
--resolution-precision`). Measured without the correction, dropping the
receiver rows read as a regression from 92.4% to 85.1% at `high` on flask; it
was the instrument that had been rewarding the defect. An `INSTANTIATES` edge
answers a call site as a `CALLS` edge does.

The rows marked "before" above were adjudicated with the corrected method, on
an index the earlier build wrote. Tables elsewhere in this document predate it.

### The record's wrong bindings, one by one

| Wrong binding in the record | Now |
|---|---|
| Go: `errors.New`, `log.New`, `template.New` bound to gin's `New`; `http.Dir` to `gin.Dir` | unbound, recorded external |
| Go: `require.Error` (87 times) bound to a gin method | unbound, external (a declared dependency) |
| Go: `c.Request.Header.Set` bound to `(*Context).Set` | unbound (ambiguous) |
| Go: `w.Header().Get` bound to the package variable `binding.Header` (80 edges) | unbound: a variable is not a candidate. `Header` is external where `w` is declared an `http.ResponseWriter` and ambiguous where `w` has no stated type |
| Go: `root.addRoute` bound to the enclosing `(*Engine).addRoute` | bound to `(*node).addRoute`, which is right |
| Go: `engine().LoadHTMLGlob(p)` bound to the enclosing function | bound to `(*Engine).LoadHTMLGlob` at `low`, which is right |
| Go: `cn.CloseNotify()` on an `http.CloseNotifier` bound to itself | still wrong, differently: bound at `low` to a test double, the only other `CloseNotify` |
| Java: local `reader` stored as a call and bound to `reader(String)` (1,025 edges) | no such reference; `reader.peek()` binds to `JsonReader.peek` by the declared type |
| Java: `strings.add(..)` on a `java.util.List` bound to a test class's `add` | unbound, external |
| Java: `asBigDecimal().intValue()`, `super.clone()` bound to the enclosing method | unbound |
| Java: first-declared overload chosen | unchanged in what is chosen; now labelled `low` unless the argument count selects one |
| TypeScript: `util.joinValues` in v4 bound to `v3/helpers/util.ts` (121 edges) | bound to `v4/core/util.ts`: the import specifier is resolved |
| TypeScript: `Object.defineProperty`, `Reflect.deleteProperty` bound to a proxy handler | unbound, external |
| TypeScript: `Array.map`, `types.includes(...)` bound to zod methods | unbound |
| Python: an edge to the object `client` for `client.get("/")` | no such reference |
| Python: `_json.dumps` bound to flask's own `dumps` | unbound, external |
| Python: builtin `open()` bound to `FlaskClient.open` | unbound |
| Python: `auth.login()` on a fixture bound to the view `login` at `high` | unbound (ambiguous): a declared value is not read as a module |
| Ruby: `String.new`, `Mutex.new` bound to one `def self.new` (44 edges) | unbound, external |
| Ruby: `respond_to?` (61) and `to_s` (41) bound to one class's methods | unbound |
| Rust: `self.inner.foo()` inside `fn foo` bound to itself (193 self-loops) | 22 self-loops remain, all `self.f()` or bare recursion; the field's declared type decides |
| Rust: `SearcherBuilder::new().line_number(false)` bound to a test `fn line_number` | unbound (ambiguous) |
| Rust: 24% of call edges pointed at a struct, enum or dependency | 0 |
| Rust: `messages::errored()` produced an edge to a function `messages` | no such reference |
| C/C++: `parser->callbacks.error(p_ctx)` bound to a function `error` | unbound |
| C++: `iter->Next()` bound to the only override in the file | unbound: the type is declared in a header the index does not hold |
| A C macro bound to a JSON key, libc `write` to a Python function, `req.text()` to a Prisma column, `importlib.metadata.version()` to a YAML key, JavaScript calls to `package.json` lines | unbound: not callable, or not a language the caller can call |

Each is pinned by `fixtures/receivers`, a hand-written tree per language whose
marked calls `TestReceiverFixtureBindsNothingWrongly` checks end to end.

### What is still wrong

- Overloads are not resolved. Where a type declares several methods of one name
  and the argument count does not single one out, the first declaration is
  bound, at `low`: 4,065 edges on gson, Newtonsoft.Json and leveldb, 49 right of
  96.
- A value whose type no declaration states is bound by name or not at all.
  `router := New()` leaves `router` untyped: return types are not read. On gin
  that is 176 of the call sites `scip-go` resolves and this does not.
- A member called on a standard-library interface from inside a wrapper of the
  same name binds, at `low`, to whatever other member of that name the
  repository has (`cn.CloseNotify()`).
- "External" is claimed from imports, manifests and directories. An import
  whose package the repository vendors under another path is recorded external.
  Python modules that share a name with any file in the repository (`typing`
  where `flask/typing.py` exists) are not, and stay plain unresolved.
- Ruby binds a bare call inside a block by what the file requires. That
  measured 96%, and it is a statement about sinatra, whose DSL makes it true.
- C# and Java members the syntax tree leaves outside their class are given the
  class declared before them. A nested type's members after such a break are
  attributed to the outer type.
- Nothing here was measured on PHP, Kotlin, Scala, Swift or Dart, whose rows in
  the trait table are written from the languages' rules and not from a sample.
  PHP, Kotlin and Scala still extract no references at all. *(Superseded: PHP,
  Kotlin and Scala record calls since [12 November](#2026-11-12-calls-in-php-kotlin-and-scala-and-new-in-java),
  and PHP, Kotlin and Scala were read by hand in the
  [second verification](#2026-11-13-the-second-verification-and-what-is-still-wrong).)*

### Cost

Index plus resolve on zod (743 files), three runs each, same machine:

| | Index | Resolve |
|---|---|---|
| Before | 5.41, 5.48, 5.98 s | 1.23, 1.25, 1.30 s |
| After | 5.30, 5.54, 5.56 s | 1.40, 1.41, 1.45 s |

The tree walk replaces three queries per file, which is why indexing did not
slow. Resolution reads two more things per run, every file path and every
declared dependency, and nothing per reference. `tests/scale` passes unchanged.

## Method

```bash
repocortex index && repocortex resolve
scip-go --output index.scip          # compiler ground truth
repocortex scip import index.scip      # lands as REFERENCES/exact/compiler
repocortex doctor --resolution-precision
```

A heuristic `CALLS` edge is adjudicated by asking the compiler what the
reference at that source position resolves to. Agreement means both name the
same definition.

**Judgement is restricted to lines the compiler resolved to exactly one
target.** That restriction is the load-bearing methodological choice, and it
exists because neither looser option is sound:

- *Matching by line alone is too generous.* On RepoCortex's own source, 25.8% of
  referenced lines carry more than one distinct target, so a heuristic edge
  could be scored against a reference it was never about.
- *Matching by line and column is too strict.* The two tools do not share a
  column convention: for `svc.Method()` the resolver records the column of
  `svc` and `scip-go` records the column of `Method`, and the gap varies with
  the receiver's name. Requiring exact columns discarded two thirds of the
  comparison (5,805 judgeable edges fell to 3,541) without either tool being
  wrong.

On unambiguous lines the answer is defined whatever either tool believes about
columns. Everything else is reported as uncovered rather than scored — a
measurement that quietly counts cases it cannot decide is worse than a smaller
one that says so.

**The compiler's one target must be the callee.** Added with the receiver
change, and described under "A correction to the adjudication" above: a line
whose single compiler fact is about the receiver of a call says nothing about
what the call reaches, and an edge is compared only with a fact about its own
name. The tables in the sections below were computed before this restriction
and include lines it now leaves out.

## What this is not

**It is not recall** — but recall *is* now measured, separately, by
`repocortex accuracy`. This paragraph used to end the discussion, and it was half
right.

The objection is sound as an objection to the **denominator**: SCIP records that
one symbol *references* another, not that it *calls* it, so dividing by every
SCIP reference penalises the resolver for correctly declining to emit a `CALLS`
edge at a type annotation. What the paragraph then concluded — that a suitable
ground truth does not exist — was wrong. We know which references sit at call
sites, because the parser records that independently of whether the resolver
could bind them. Restricted to lines where the parser saw a call *and* the
compiler resolved that line to exactly one definition, every element of the set
is a call whose correct target is known, and an edge we did not produce there is
a missed caller.

That restriction is the whole soundness argument, so the report states it inline
rather than in a footnote.

The measurement changes the unit: precision here counts *edges*, while a missed
call site has no edge to count, so `repocortex accuracy` collapses each call site
to one outcome and computes precision, recall and F1 over that. Every tier then
shares one denominator, tier recalls sum to the all-tiers recall, and F1 is a
harmonic mean of two fractions over the same set.

**On this repository the number that matters is the one precision could not
show**: `high` is 98.6% precise and catches **61.5%** of adjudicable calls.
2,855 of 8,315 known call sites produce no edge at all — 2,504 unresolved, 351
ambiguous. For a change-safety product that is the important half: an empty
`repocortex callers` result means *unknown*, not *none*, and roughly a third of
known calls are in that gap.

It is still not recall over the calls in the repository. The call-site oracle is
RepoCortex's own parser, so a call it never parsed is neither scored nor missed;
the alternative would be the compiler's own call graph, which SCIP does not
emit.

**It is not a claim about proven edges.** Compiler-backed edges are not scored
against themselves. They are `exact` because a typechecker produced them, and
`doctor --verify-index` enforces that nothing may claim `exact` without
`source=compiler`.

**Java is measured, later and on a small pool.** See "Java: measured" below
for the numbers, and for how small the pool is.

**Five repositories is still a small sample**, and one of them is this
project's own code, which the resolver was developed against — repocortex's 96.1%
is the least trustworthy number in the table for that reason, and it is the
highest. Prometheus, nest and scrapy are third-party code the resolver has
never been tuned against, and they agree with each other at 86.8%, 87.0% and
87.1%.

## Python: now declared, not inherited from defaults

Python used to have no declared resolution behaviour at all. It appeared in none
of the three language switches the resolver carried, so it silently took the
fallback branch of each — not a decision anyone made, just the shape of an `if`.

It also had a parse gap that made the fallback worse than it looks. Python is a
file-is-a-module language, so the resolver consults imports *before* proximity —
but `internal/parse/queries.go` captured no relative imports, so

```python
from .helpers import X
from ..pkg.mod import Y
```

produced **zero** imports. A probe on a six-import file captured three. A module
importing only relatively therefore had no import tier at all, and fell straight
through to the tier the ordering exists to demote.

The added capture is one pattern:

```
(import_from_statement module_name: (relative_import (dotted_name) @path))
```

Measured against `scip-python` 0.6.6, both corpora `--depth 1` clones, the same
SCIP index used for both arms of each pair:

| corpus | arm | `high` | judged | agree | disagree | imports |
|---|---|---:|---:|---:|---:|---:|
| scrapy (480 files) | without relative imports | 95.4% | 5,259 | 5,017 | 242 | 4,244 |
| scrapy | **with** | 95.4% | 5,275 | 5,033 | **242** | 4,266 |
| requests (37 files) | without relative imports | 95.3% | 148 | 141 | 7 | 233 |
| requests | **with** | **95.6%** | 226 | 216 | 10 | 299 |

Both clear the documented `high` floor of 86.8% comfortably. On scrapy the change
added 16 judgeable high-tier edges and **every one of them was correct** —
`disagree` is unmoved at 242.

**One number moved the wrong way and is not a regression.** On requests the
`medium` tier fell from 92.6% (81 judged) to 60.0% (20 judged), and all-tiers from
94.3% to 92.7%. That is composition, not accuracy: 61 correct edges were
*promoted* out of `medium` into `high`, leaving the weaker ones behind in a much
smaller pool. The tier the product tells you to act on improved on both corpora,
which is the tier the number is for. It is recorded here because a tier moving
30pp is exactly the kind of thing that looks like a defect six months later if
nobody wrote down why it happened.

## Python: the receiver's class, read from the source

Until this change a Python method call was resolved by its NAME. `self.save()`
bound when the file happened to contain exactly one `save`; `account.save()`
bound to that same `save` whatever `account` was; and a class imported from the
package that re-exports it — which is how a library's public names are imported
— was found only by the last-resort tier that asks whether a name is unique in
the repository. The parser knew none of the three things that would have
settled those: which class a definition belongs to, what class a receiver is,
and which name an import brought in.

`internal/parse/pyscope.go` now states them, and
`internal/resolve/infer/static.go` uses them.

**What the parser states.**

- A definition's qualified name follows Python's own `__qualname__`:
  `shop/models/account.py:Account.save`, and `build.<locals>.Item.render` for
  anything defined inside a function. Two classes' `save` are two names.
- A receiver is replaced by a class where the source says what it is, and
  nowhere else: a parameter annotated `account: Account` (also `Optional[...]`,
  `X | None`, the quoted and dotted forms), or a variable the function assigns
  exactly once from `Ledger(...)`. A second assignment, a loop target, a `with
  ... as`, an unpacking or a `global` anywhere in the function withdraws it.
  Nothing reads a function's return type, an attribute's type or a module-level
  variable's.
- A call on an expression with no name — `self.repo.save()`, `make().run()` —
  is recorded as made on `?`, so that an empty receiver means the source wrote a
  bare name. `super().save()` is recorded as made on `super`.
- A call through a local value — a parameter, a loop variable — is marked, and
  is bound to nothing.
- `from shop.models import Account` records that `Account` came from
  `shop.models`, and a relative import keeps its dots.

**What the resolver does with them.**

- `self.x()`, `cls.x()`, `Account.x()` and a receiver the parser replaced with a
  class are asked about THAT class first, ahead of the same-file tier: the
  method the class declares, and failing that the nearest one an ancestor
  declares.
- A bare call cannot name a method, and a definition local to a function is
  visible only inside it. Those candidates are removed, and among what is left
  the innermost definition wins.
- A name imported by name resolves through the import that named it: to the
  module if the module defines it, and otherwise to the one definition of that
  name beneath the package — a re-export. `models.CharField(...)` after
  `from django.db import models`, and `httpx.Client()` after `import httpx`,
  resolve the same way.
- `shop/models/__init__.py` is the module `shop.models`.

**What was taken out of `high`.** A method called on an object whose class
nothing states — `target.save()`, `self.repo.save()` — used to bind to the only
`save` in the file, or the only one in an imported module, and be reported at
`high`. Those bindings are still made. They are reported one level down, as
what they are: a binding on a name. The same-directory tier is off for Python
entirely. The measurement behind both is under "What the old tiers were worth".

### Measured against hand-written ground truth

`fixtures/pycalls` is a small package in which every interesting call site
states, in a comment, the definition it reaches, or that it is dynamic and
nothing may claim it at `high`: 57 call sites with a known target and 13
dynamic ones. It covers the shapes a compiler index cannot adjudicate — the
target of a call through a parameter is a local value, which SCIP does not
record — and it is a regression gate (`TestPythonCallGroundTruth`).

| | `high`: right | `high`: wrong | all tiers: right | wrong | no edge | dynamic calls claimed at `high` |
|---|---:|---:|---:|---:|---:|---:|
| before | 26 | **9** | 36 | 9 | 12 | **4 of 13** |
| after | **55** | **0** | 55 | 1 | 1 | **0 of 13** |

The one wrong edge left is `service.charge(...)` where `service` is what a
function returned: the receiver's class is unknown, the name-only tier picks the
one `charge` in an imported module, and that is the subclass's override. It is
reported at `medium`. The one miss is `Tree.Node().walk()`, a call on the
result of a call.

### Measured against a compiler index

Four repositories, each a `--depth 1` clone of a pinned commit, each indexed
once with `scip-python` 0.6.6 and that one index used for both arms. Figures are
`repocortex accuracy` output, unedited: call sites the parser recorded a call on
and the compiler resolved to exactly one definition.

| repository | call sites | arm | `high` sites | right | wrong | `high` precision | `high` recall | all-tier precision | all-tier recall | no edge |
|---|---:|---|---:|---:|---:|---:|---:|---:|---:|---:|
| scrapy `c7eacdbb` (480 `.py`) | 9,999 | before | 5,230 | 5,017 | 213 | 95.9% | 50.2% | 93.1% | 63.9% | 3,132 |
| | | **after** | 6,732 | 6,709 | 23 | **99.7%** | **67.1%** | 96.2% | 72.6% | 2,446 |
| requests `v2.32.3` (36) | 352 | before | 293 | 283 | 10 | 96.6% | 80.4% | 95.0% | 80.4% | 54 |
| | | **after** | 300 | 295 | 5 | **98.3%** | **83.8%** | 96.4% | 83.8% | 46 |
| httpx `0.28.1` (60) | 1,826 | before | 405 | 376 | 29 | 92.8% | 20.6% | 54.1% | 31.7% | 755 |
| | | **after** | 1,293 | 928 | 365 | 71.8% | 50.8% | 67.0% | 51.6% | 420 |
| django `5.1.4` (2,788) | 44,787 | before | 17,996 | 15,443 | 2,553 | 85.8% | 34.5% | 78.7% | 40.6% | 21,667 |
| | | **after** | 30,359 | 27,579 | 2,780 | 90.8% | 61.6% | 86.3% | 63.0% | 12,084 |

Two rows in that table do not say what they appear to. On httpx `high`
precision FELL, from 92.8% to 71.8%, and on django it reached only 90.8%. Both
are the compiler index being wrong, and the next section is the evidence for
saying so rather than a reason to look away from the number.

One row says something about the baseline: on django, before this change,
`high` measured **85.8%** — under the 86.8% floor this document publishes for
the tier.

### The compiler index is wrong about wildcard re-exports

`httpx/__init__.py` is twelve lines of `from ._models import *`. A test that
writes `httpx.Cookies()` reaches the class `Cookies` in `httpx/_models.py`.
`scip-python` 0.6.6 records that occurrence as a reference to
`` `httpx._models`/Request# `` — a different class in the same module — and does
the same for every name that arrives through a wildcard import: every
`models.CharField(...)`, `models.IntegerField(...)` and `models.BooleanField(...)`
in django's tests is attributed to `DateTimeField`. Read from the SCIP file
directly, not from RepoCortex's import of it:

```
tests/models/test_cookies.py:24   cookies = httpx.Cookies()
  [23 14 19]  scip-python python … httpx/__init__:
  [23 20 27]  scip-python python … `httpx._models`/Request#
```

So an edge that names the right class is scored wrong, and the better the
resolver gets at these calls the lower its measured precision goes. The
baseline's precision on httpx was high because it bound almost none of them at
`high`; it bound them at `low`, where 424 of 618 were scored wrong for the same
reason.

This was checked with something that depends on neither tool. For every call
site where an edge and the compiler index disagree and the call is a bare name
or is qualified by a name the file imported, the import chain is followed with
Python's own `ast` module — `from … import *` and `__all__` included — and the
site is counted right only if the chain ends exactly where the edge points. A
second, smaller class is corrected the same way: where an edge names a function
defined inside the function the call is written in, and the compiler index names
a same-named function defined inside a DIFFERENT function, Python's scoping
makes the compiler's answer impossible. Nothing else is touched. Both
corrections are applied to both arms, whatever tier and reason an edge carries.

| repository | arm | verdicts corrected | `high` right | wrong | `high` precision | `high` recall | all-tier precision | all-tier recall |
|---|---|---:|---:|---:|---:|---:|---:|---:|
| scrapy | before | 0 | 5,017 | 213 | 95.9% | 50.2% | 93.1% | 63.9% |
| | **after** | 17 | 6,726 | 6 | **99.9%** | **67.3%** | 96.4% | 72.8% |
| requests | before | 0 | 283 | 10 | 96.6% | 80.4% | 95.0% | 80.4% |
| | **after** | 0 | 295 | 5 | **98.3%** | **83.8%** | 96.4% | 83.8% |
| httpx | before | 358 | 376 | 29 | 92.8% | 20.6% | 87.5% | 51.3% |
| | **after** | 365 | 1,293 | 0 | **100.0%** | **70.8%** | 93.0% | 71.6% |
| django | before | 842 | 15,573 | 2,423 | 86.5% | 34.8% | 82.4% | 42.5% |
| | **after** | 2,725 | 30,299 | 60 | **99.8%** | **67.7%** | 94.7% | 69.1% |

Read the first table as the measurement and the second as what it means. The
first is what the tool prints; the second is what remains after 3,107 verdicts
the index got wrong are set right by a method anyone can re-run. Neither is
offered without the other.

A third artefact is left in both tables because it cuts both ways and cannot be
corrected, only counted. A call site is adjudicated per LINE, so a line holding
two calls is scored against whichever one the compiler resolved. On requests all
5 `high` sites still scored wrong are of that kind — the compiler's one target
on the line has a different name from the call that was bound — and on scrapy 3
of the remaining 6 are.

### What each rule is worth

Edge by edge rather than site by site, over the four repositories together,
after the two corrections above. The last column is the same figure as the
compiler index scores it.

| tier | why the edge was made | django | scrapy | httpx | requests | all four | agreed | as indexed |
|---|---|---:|---:|---:|---:|---:|---:|---:|
| `high` | imported by name from the module that defines it | 5,584 of 5,584 | 2,318 of 2,320 | 134 of 134 | 135 of 138 | 8,171 of 8,176 | 99.9% | 99.8% |
| `high` | inherited: declared on an ancestor of the receiver's class | 6,650 of 6,714 | 799 of 805 | 49 of 49 | 7 of 7 | 7,505 of 7,575 | 99.1% | 99.1% |
| `high` | imported by name from a package that re-exports it | 5,252 of 5,273 | 789 of 789 | — | — | 6,041 of 6,062 | 99.7% | 99.5% |
| `high` | declared on the enclosing class (`self`, `cls`) | 4,544 of 4,545 | 1,345 of 1,348 | 88 of 88 | 67 of 67 | 6,044 of 6,048 | 99.9% | 99.9% |
| `high` | qualified by an imported package that re-exports it | 4,122 of 4,122 | 8 of 8 | 576 of 576 | 0 of 6 | 4,706 of 4,712 | 99.9% | 36.2% |
| `high` | defined in the same file (a bare name) | 2,727 of 2,729 | 911 of 911 | 187 of 187 | 80 of 80 | 3,905 of 3,907 | 99.9% | 97.6% |
| `high` | declared on the receiver's class (annotation, constructor, or the class written out) | 978 of 978 | 518 of 518 | 259 of 259 | 5 of 5 | 1,760 of 1,760 | 100.0% | 100.0% |
| `high` | qualified by an imported module that defines it | 366 of 367 | 15 of 17 | — | 1 of 1 | 382 of 385 | 99.2% | 99.2% |
| `high` | defined in the enclosing scope, nearest of several | 84 of 84 | 23 of 24 | — | — | 107 of 108 | 99.1% | 98.1% |
| `high` | receiver spells a module's file name | 0 of 3 | — | — | — | 0 of 3 | 0.0% | 0.0% |
| `medium` | one match in an imported module; receiver's class unknown | 420 of 1,204 | 397 of 483 | 6 of 13 | 0 of 2 | 823 of 1,702 | 48.4% | 48.4% |
| `medium` | one match in the same file; receiver's class unknown | 49 of 207 | 37 of 118 | 4 of 17 | 0 of 6 | 90 of 348 | 25.9% | 25.9% |
| `medium` | the class does not declare it, its ancestors did not settle it; one match by name | 89 of 109 | 10 of 11 | — | — | 99 of 120 | 82.5% | 82.5% |
| `medium` | one match in an imported module; a bare name the file never imported | 28 of 46 | — | — | 0 of 2 | 28 of 48 | 58.3% | 58.3% |
| `low` | several matches among imports; receiver's class unknown | 8 of 1,227 | 110 of 237 | 4 of 24 | 0 of 1 | 122 of 1,489 | 8.2% | 8.2% |
| `low` | unique name in the repository, no import evidence | 64 of 100 | 0 of 12 | 0 of 66 | — | 64 of 178 | 36.0% | 33.1% |
| `low` | the class does not declare it, its ancestors did not settle it; several matches by name | 0 of 4 | — | — | — | 0 of 4 | 0.0% | 0.0% |

Over all four, `high` is 38,621 of 38,736 edges, **99.7%** — 91.7% as indexed.
`medium` is 46.9% and `low` 11.1%, which is what those two labels say they are.

The six `high` edges on requests the correction could not reach are
`requests.get(...)` and `requests.Request(...)` in a `src/`-layout repository:
the lines are among the "different name" artefacts above, and the `ast` walk
does not look under `src/`. They are left as the index scores them.

### What the old tiers were worth

The relabelling was decided by this table. It was taken on an intermediate
build, before the labels moved, on scrapy and django, and counts edges for a
method called on an object whose class nothing states:

| what bound it, at `high` | scrapy | django |
|---|---:|---:|
| the only definition of the name in the file | 37 of 113 (33%) | 48 of 168 (29%) |
| the only one in an imported module | 393 of 479 (82%) | 394 of 1,057 (37%) |
| several among the imports (then `medium`) | 110 of 235 (47%) | 7 of 1,098 (1%) |

None of the three clears 86.8%, and on django the first two were most of the
reason `high` measured 85.8%. They are `medium` and `low` now, and the final
table above is the same population after the move.

The same-directory tier, which reports `medium` where a language requires an
import, was right **1 time in 213** on Python across scrapy, requests and httpx:
a Python name a file neither defines nor imports cannot denote a neighbour's
definition. It is off for Python (`LanguageSpec.ProximityIsEvidence`), and a
reference only it could bind is recorded ambiguous.

A call on `super()` whose class has no indexed ancestor declaring the method
used to fall back to the only definition of the name in the file — which is,
more often than not, the method the call is written in. Measured on django
before that fallback was removed: right 0 times in 54.

### Arity for Python: measured, and removed

A Python arity query was built for the same run: a method's `self` taken off the
count, and a signature with a default, a splat or a bare `*` refused outright,
as a call passing a splat is. One binary with it and one without, same trees,
same indexes.

| repository | `high` | `medium` | `low` right, with arity | `low` right, without |
|---|---|---|---:|---:|
| scrapy | identical | identical | 109 | 110 |
| django | identical | identical | 66 | 67 |
| requests, httpx | identical | identical | identical | identical |

It chose between candidates 56 times, all in the lowest tier, and was right
once; without it two more call sites were right. That is the TypeScript result
again (see "A4 on TypeScript" below) for the same reason — arity only ranks
candidates a tier has already accepted, and in Python those are the name-only
bindings for an unknown receiver — and it got the same decision. The query, its
dialect and the method bookkeeping were deleted.

### The tier

`repocortex languages` now lists Python as `typed`. The tier is derived, not
listed (`internal/parse/tiers.go`), and until now the derivation had one route:
import, receiver and arity queries. Python has no arity query, for the reason
just given, so the derivation gained a second route — a scope analysis that
states the receiver's class — and the tier's description changed from "receiver
and arity discrimination" to "receiver discrimination by arity or class".

That is a change to what the word means, made on these numbers:

| | `high` precision | `high` recall |
|---|---:|---:|
| Python, four repositories, as indexed | 71.8 – 99.7% | 50.8 – 83.8% |
| Python, four repositories, corrected | 98.3 – 100.0% | 67.3 – 83.8% |
| Go (own source) | 98.6% | 60.4% |
| TypeScript | 99.5% | 66.4% |
| Java (spring-petclinic-rest) | 100.0% | 32.7% |

If the correction is not accepted the as-indexed row is the one to read, and on
it Python clears the 86.8% floor on three repositories of four and not on httpx.
The promotion rests on the corrected row, and on the correction being checkable.

### Cost

django, 2,788 Python files, `index --full` then `resolve`, the two binaries
alternated within each of three rounds on one machine:

| phase | binary | wall clock, three rounds | CPU, three rounds |
|---|---|---:|---:|
| `index --full` | before | 30.4 – 34.6 s | 73.8 – 88.2 s |
| | after | 24.2 – 35.9 s | 63.4 – 85.7 s |
| `resolve` | before | 6.8 – 8.9 s | 7.6 – 9.7 s |
| | after | 8.5 – 11.5 s | 9.2 – 12.4 s |

The machine was doing other work throughout, which is why the wall clock ranges
overlap and the CPU column is the one to compare. Indexing is unchanged within
that noise: 6,817 files discovered, 2,340 indexed, 41,688 symbols in both arms.
`resolve` costs about a quarter more CPU — round by round 9.7 to 12.4 s, 7.6 to
9.7 s and 7.7 to 9.2 s — and most of it is the hierarchy lookups for parked
references and the SQLite writes for edges it did not use to find. On scrapy
(480 files) neither phase moved outside the noise.

Resolution writes 32% more edges on that tree (57,015 to 75,129) and 24% fewer
reference rows are stored (253,125 to 192,001: the object of `row.save()` is no
longer recorded as a call to `row` when `row` is a local value). Two costs were
found by profiling and removed before these figures were taken: the scoping
filter re-derived a reference's kind once per candidate, and the hierarchy pass
asked the registry for the same class's supertypes once per chunk of parked
references rather than once per run.

### What this does not do

- **A receiver whose class is only inferable.** `crawler = get_crawler()`,
  `self.settings.getbool(...)`, a pytest fixture: 1,088 of scrapy's 2,446
  remaining misses are a method on such a receiver. Reading return annotations
  and attribute types would reach some of them; neither is done.
- **Method resolution order.** Two bases that both declare a method leave the
  call ambiguous, because the hierarchy is stored by name and unordered. Python
  would take the leftmost.
- **A class identified by name alone.** The hierarchy is keyed by class NAME.
  A receiver's own class is pinned to one definition; its ancestors are not, so
  two unrelated classes called `Base` share their ancestry.
- **Aliased members.** `from m import a as b` is not followed.
- **The denominator.** About 1,200 of scrapy's remaining misses are lines where
  the compiler's one target has a different name from every call the parser
  recorded there — `isinstance(x, Response)` resolves `Response`, and
  `isinstance` is a builtin. No call resolver can produce that edge. They count
  as misses for both arms, as they did before.

## Measured on a polyglot repository that is not ours

Every figure above this section was measured on Go, on corpora chosen for
measurement. The obvious objection is that a resolver tuned on Go, measured on
Go, reports Go's numbers — so this section reports the tiers on somebody else's
codebase, in languages that are not Go, adjudicated against those languages'
own compilers.

**The corpus.** A production polyglot estate: 8,151 indexed files, 91,548
symbols, 69,998 relationships. 40.8% Python, 23.7% TypeScript, 8.5% Go, 6.8%
Vue, plus Java and JavaScript. Nothing about it was chosen by us and nothing in
RepoCortex was tuned on it.

**The oracles.** `scip-python` 0.6.6 on one Python service (273 of 273 documents
matched, 5,081 compiler-proven edges) and `scip-typescript` 0.3.14 on one
frontend (1,339 of 1,348 documents, 5,834 edges).

| Tier | Judged | Agree | Disagree | Precision |
|---|---:|---:|---:|---:|
| `high` | 3,534 | 3,395 | 139 | **96.1%** |
| `medium` | 50 | 0 | 50 | **0.0%** |
| `low` | 6 | 1 | 5 | 16.7% |
| all | 3,590 | 3,396 | 194 | 94.6% |

**`high` holds outside Go.** 96.1% here against 86.8–97.8% on the Go corpora.
That is the claim the tier ordering rests on, and until now it had only ever
been demonstrated in the language the resolver was developed in.

**`medium` is 0.0% here, and that is the more useful finding.** Fifty judged
edges, fifty wrong. On Go the same tier measures 35.4%, and 45.8% after arity
disambiguation. So "shown, never acted on" is *understated* for Python and
TypeScript on this corpus: a medium edge here is not weak evidence, it is noise.
A single global range across all languages hides that, and the per-language
split is the honest presentation. It also explains why arity disambiguation —
which targets `medium` — is worth extending beyond Go.

### What this does NOT establish

**Coverage is 5.7%** — 3,590 of 62,961 heuristic `CALLS` edges were adjudicated.
One Python service of 3,100 Python files, and one frontend. The precision figure
is sound for the edges it judged and is not a whole-repository number; the rest
are neither right nor wrong here.


## Java: measured attempt #5, and the first precise diagnosis

Earlier entries here record Java as "blocked: needs gradle". Measured against a
real 2,681-file Java application (Maven, `pom.xml`, Java 25), that description
is wrong on both halves, and the correction matters because it changes what
would have to be fixed.

**What actually happened, in the order the blockers appeared:**

1. **RepoCortex's own gate had no key.** Indexing Java is refused by default
   because it runs the repository's Maven or Gradle build, which executes
   arbitrary code from the repo. The refusal said "re-run with the explicit
   build opt-in" -- and `AllowRepoBuild` appeared nowhere outside tests. No
   flag, no documented config key. The instruction could not be followed, so
   some unknown share of "3 of 3 repos failed" was this, not the indexer.
   Fixed: `repocortex indexers install --allow-repo-build`.

2. **The documented config path is wrong on macOS.** Code comments named
   `~/.config/repocortex/config.yml`; `os.UserConfigDir()` resolves to
   `~/Library/Application Support/repocortex/config.yml` there. A config written
   to the documented path is read by nothing, so the tool repeated the same
   "set accuracy.indexers.runner" instruction after it had been set. Fixed.

3. **The pinned indexer image is older than modern Java.** With the first two
   cleared, `scip-java` ran cpam's Maven build for 7 minutes and exited 1:

       javac 17.0.16
       error: release version 25 not supported

   The pinned image ships JDK 17. Any project targeting Java 21 or 25 is
   unindexable with it, regardless of build tool.

**So the honest statement is: the oracle path works; the pinned image is too
old.** `scip-java` handled Maven without complaint -- gradle was never the
obstacle on this repository. Java precision was still unmeasured when this was
written, and this section said no Java number should be quoted until a JDK
21/25 image was pinned and its digest recorded. What happened instead: the
same JDK 17 image's launcher fetches a Temurin 21 for itself at start-up, and
the number in "Java: measured" below came from that image, digest recorded
there.

**What this cost, and the lesson.** Three failures, ~10 minutes of build time,
and none of the three was detectable from the error in front of us at the time.
All three were knowable BEFORE running anything: a flag either exists or does
not, a config path either resolves or does not, and a project's
`maven.compiler.release` can be compared against the pinned image's JDK without
compiling a line. A tool that only reports incompatibility after a seven-minute
build is making the user run the experiment we should have run for them.

**Java was still unmeasured when this was written; it has since been measured
— see "Java: measured" below. Both the "until a JDK 21/25 image is pinned"
condition above and the reason recorded next are superseded by that
section.** `scip-java`
reported `blocked: needs gradle` on all four Java targets in this repository —
a fourth independent repository where compiler-backed Java indexing did not run.

### How the rest of the field handles this, and why it is not the same problem

The obvious question about a `blocked: needs gradle` is what everyone else does
instead. From the eight teardowns in `docs/internal/research/`: **not one of them uses a
compiler for Java. Not one of them uses a compiler for any language.** There is
no SCIP import, no LSIF, no language server anywhere in the set — the single
mention of LSP is mcp-codebase-index arguing against it on install cost
(`README.md:277`).

They all parse Java with tree-sitter or regex and emit edges from it. That is a
legitimate engineering choice and it is the same choice RepoCortex makes when no
indexer runs. The difference is what happens to the number afterwards.

CodeGraph binds a Java field by name suffix — `endsWith('Service')` emits an
edge at **confidence 0.85**, and `/^[A-Z][a-zA-Z]+$/` emits at **0.70**, which
fires on every PascalCase identifier in the language (`frameworks/java.ts:129-190`).
Those constants are not measurements. Nothing adjudicated them, and there is no
oracle in that project against which they could be. Its own sound alternative —
`inferJavaFieldReceiverType`, which reads the declared field type — sits unused
in the same codebase at `name-matcher.ts:1060`.

So the honest comparison is not "our Java is blocked and theirs works". It is:

  - **RepoCortex's Java was UNMEASURED when this comparison was written.** It
    was indexed, edges were produced, and they carried `high`/`medium`/`low` —
    but no compiler had adjudicated them, so this document said NOT MEASURED
    and the per-language table said it on every run. It has since been
    measured, on 82 edges — see "Java: measured" below.
  - **Theirs is UNMEASURABLE.** They ship a Java confidence number derived from
    a name suffix, with no oracle in the project that could ever check it.

The gap that matters is not the gradle dependency. It is that a tool which
cannot say how accurate it is on a language will also never discover that it is
wrong on that language. `scip-java` failing on four of four repositories is a
real limitation and it is recorded as one; a 0.85 that nothing produced is a
different kind of problem, and it does not show up as a limitation anywhere.

**`repocortex accuracy` took about twenty minutes on this repository when these
figures were first produced. It now takes 4 seconds, for byte-identical output.**

Two joins had nothing to stand on. `edges(evidence_file, evidence_line)` had no
index at all — every other access path on that table is by symbol id, and
nothing had ever needed to look an edge up by location — so adjudicating a call
site against the compiler's edge at the same file and line scanned every edge in
the run. `symbol_references(file_id, start_line)` likewise.

The indexes were worth 2x. The rest was the query SHAPE. It built a `callsite`
CTE of every parsed call site — 305,237 rows — and joined it to the
compiler-adjudicated set of 5,587. SQLite cannot index a CTE, so the plan read
`SCAN c` then `SCAN u`: a nested loop over the product. Inverted so the small
side drives, each of the 5,587 lines does two indexed probes instead. 160s to
0.16s on that query alone.

Recorded because a measurement nobody runs stops being true without anyone
noticing, which is the exact failure this whole document exists to prevent.

### Recall, per language

Precision says how often an edge is right. It says nothing about the calls never
found, and for a change-safety product a missed caller is the expensive error.
Restricted to call sites the compiler resolved to exactly one definition:

| | `high` precision | `high` recall | F1 | adjudicated sites | produced no edge |
|---|---:|---:|---:|---:|---:|
| **Python** (before the scope analysis) | 94.2% | 56.3% | 70.4% | 3,277 | **1,306 (39.9%)** |
| **TypeScript** | 99.5% | 66.4% | 79.6% | 2,330 | **749 (32.1%)** |
| Go (own source) | 98.6% | 60.4% | 75.0% | 8,315 | 2,855 (34.3%) |
| Java (spring-petclinic-rest) | 100.0% | 32.7% | 49.2% | 251 | **169 (67.3%)** |

**TypeScript is the most precise tier measured anywhere in this project** —
99.5%, eight wrong edges in 1,554. Python is the least at 94.2%, and both clear
the 86.8% floor.

The Python row was taken on a private service that cannot be re-measured, and
it predates the scope analysis. What Python measures now, on four public
repositories, is in "Python: the receiver's class, read from the source" above:
`high` precision 98.3–100.0% and recall 67.3–83.8% once the compiler index's
errors on wildcard re-exports are corrected, and 71.8–99.7% and 50.8–83.8% as
the index scores it.

**The recall gap is consistent across Go, Python and TypeScript**: between a third and
two fifths of known call sites produce no edge at all. That number is the same
shape in Go, Python and TypeScript, which says it is a property of heuristic
resolution rather than of any one language's grammar — and it is the number a
consumer needs, because it is what makes an empty `repocortex callers` result mean
*unknown* rather than *none*.

Java, measured later and on a much smaller pool (82 judged edges, 251
adjudicated sites), is the outlier on recall: two thirds of its known call sites
produce no edge, at a precision of 100.0%. The measurement and its caveats are
in "Java: measured" below.

**`medium` is 0.0% in both languages** — 10 judged in Python, 27 in TypeScript,
every one wrong. Precision and recall agree with each other here, which they
should: the tier is contributing nothing on either measure.

## Java: measured

Java is no longer the unmeasured language. Measured against `scip-java` on
[spring-petclinic/spring-petclinic-rest](https://github.com/spring-petclinic/spring-petclinic-rest)
at commit `4cd8e1b0cd42578e882247d8801f6be5d402f118` — a single-module Maven
project on Spring Boot 4.1.1, 109 Java files, `maven.compiler.release=17`
inherited from the Spring Boot parent, no `maven.compiler.executable`
override, `mvn -q -DskipTests compile` green in 42 s on the host. That is
exactly the closing condition the previous version of this section set, and
the first repository tried that met it.

The index came from the pinned image, under the docker runner, with no change
to RepoCortex:

| | |
|---|---|
| Image | `sourcegraph/scip-java@sha256:a05564b9c9951660fc7069b431f1a18f4575b570b7ce09a14936729832c1fc8d` (linux/amd64, run under emulation on arm64) |
| `scip-java version` | `HEAD-20260402-1456` — a snapshot build, not the `0.12.2` the tag claims |
| Toolchain inside | `javac 17.0.16` is the image's default; scip-java's own launcher fetches Temurin 21.0.12.1 through coursier at start-up ("Using JVM version '21'"). Maven's `.m2` honours the `HOME=/out` the runner sets. The JDK download did not: coursier is a JVM program and reads `user.home`, which is `?` for a uid the image has no passwd entry for, so on this measurement 611 MB of Temurin landed in the repository's working tree as `?/.cache/coursier`, untracked and not ignored. The runner now also passes `JDK_JAVA_OPTIONS=-Duser.home=/out` (every JDK 9+ launcher reads it, and it leaves the image's own `JAVA_TOOL_OPTIONS` alone), so the JDK cache goes to the output mount with everything else and a re-index leaves `git status` clean. The runner then prunes that mount before returning — Maven's `.m2`, coursier's archives, anything that is not an index — and removes it outright if no index was produced, so what outlives a run is the `.scip` file and nothing else. Both verified by re-running this indexing after the fix |
| Build driven | `clean verify -DskipTests -Dmaven.test.skip=true ...` as `java.go` constructs it |
| Wall clock | 3 m 27 s from `docker run` to a 2.8 MiB `java.scip` |

Adjudicated with `repocortex accuracy` after `repocortex scip import`, excerpted,
unedited — the block omits the tool's preamble:

```
JAVA
  TIER    SITES  RIGHT  WRONG  PRECISION  RECALL  F1
  high    82     82     0      100.0%     32.7%   49.2%
  medium  0      0      0      —          0.0%    0.0%
  low     0      0      0      —          0.0%    0.0%
  all     82     82     0      100.0%     32.7%   49.2%
  missed  169    —      —      —          —       —
  251 adjudicated call site(s); 169 produced no edge at all.

WHERE THE MISSES COME FROM
  CAUSE       CALL SITES  MEANING
  unresolved  156         the reference was bound to nothing
  ambiguous   13          several candidates and no way to choose, so no edge was claimed

COVERAGE
  Adjudicated 251 of 1,582 parsed call site(s) (15.9%). The rest sit on lines
  no compiler fact covers, and are neither right nor wrong above.
  Against 1,010 compiler fact(s) on 513 unambiguous line(s); 214 ambiguous
  line(s) were excluded. The resolver produced 624 heuristic CALLS edge(s) overall.

LANGUAGES
  LANGUAGE  FILES  CALL SITES  CALLS EDGES  COMPILER FACTS  ADJUDICATED  STATUS
  java      109    1,582       624          1,010           251          measured
```

**`high` clears the 86.8% floor — at 100.0%, 82 of 82 — and it is the only
tier Java has.** `medium` and `low` adjudicated zero call sites, so they are not
0% and not wrong; they are absent, and this table cannot say anything about
them. The recall side is the one to read carefully: **169 of 251 adjudicated
call sites (67.3%) produced no edge at all**, against a third to two fifths for
Go, Python and TypeScript. 156 of those are `unresolved`, 13 `ambiguous`, and
none named a wrong definition — the resolver is silent on Java far more often
than it is mistaken, which is the right failure for a change-safety tool but
twice the silence measured anywhere else. Two caveats keep this from being
quoted as "Java: 100%". The judged pool is small — 82 edges, against 1,554 for
TypeScript and 5,275 for scrapy — and one wrong edge would move it to 98.8%.
And only 15.9% of parsed call sites were adjudicated at all, because
`scip-java` attributes 3,748 references to a file rather than a definition and
the import skips them, so the number describes that slice, not the repository.
The recall table above now carries a Java row with both caveats attached.

### Earlier attempts

This section used to read "attempted, not measured", and the reason was
recorded because it is a property of the product and not only of the effort:
**compiler-backed indexing for Java runs the repository's real build, so it
inherits every constraint that build has.** TypeScript and Python were
straightforward by comparison — `repocortex indexers install --yes` provisioned
`scip-typescript` and `scip-python` and both produced usable indexes on the
first attempt. Java did not, across three repositories and seven builds, though
`scip-java` itself worked when pointed at a two-class Java 17 Maven project:

| repository | build | outcome |
|---|---|---|
| spring-boot | Gradle | no Gradle on the host; RepoCortex correctly reported `blocked: needs gradle` |
| commons-lang | Maven | builds, but emits no SemanticDB. `-source 8` is below what the javac plugin attaches to, and raising it hit javadoc, then checkstyle, then a pom that overrides the compiler configuration scip-java injects |
| petclinic | Maven + Gradle | needs `--build-tool=maven`; then its GraalVM Maven extension needs Java 17 while scip-java falls back through 21 → 17 → 11 and reports only the failure on 11 |

The closing condition written down then — a Maven repository on Java 11 or
later whose build succeeds with `-DskipTests` and does not override
`maven.compiler.executable` — is the one spring-petclinic-rest met. The
candidates tried on the way to it, in order, and why the script rejected each.
One caveat on the last row: petclinic-rest's release level is inherited from
the Spring Boot parent, which the script's `pom.xml` grep cannot see, so its
qualification came from `mvn help:evaluate`, not from the script:

| candidate | build | rejected because |
|---|---|---|
| spring-projects/spring-petclinic-rest | — | does not exist; the clone 404s (the project lives under `spring-petclinic/`) |
| apache/commons-csv | Maven | `maven.compiler.source` 1.8, below the javac-plugin floor |
| apache/commons-text | Maven | same, 1.8 |
| javalin/javalin-samples | Maven | the root is an aggregator POM that declares no compiler level; the script stops there |
| **spring-petclinic/spring-petclinic-rest** | Maven | **qualified** — release 17 via the Spring Boot parent |

A synthetic Java corpus was deliberately not used — uniform generated code
makes resolution easy and would report an inflated precision, which is worse
than no number.

Two RepoCortex defects fell out of the earlier attempts, both fixed before this
measurement:

- `indexers list` printed **`ready`** for a step the planner had blocked,
  because the status column never consulted `Blocked`. The blocked step is the
  Java one, withheld pending an explicit opt-in precisely because indexing it
  executes the repository's build. A safety gate the interface does not show is
  one a user works around by accident.
- The Docker runner passed `-u <uid>` without a writable `HOME`, so `HOME`
  resolved to `/` and coursier died on `AccessDeniedException: /.cache` before
  any build started. The container route is the one the planner recommends
  *instead of* running an untrusted build on the host, and it could not index
  anything.

One host condition cost thirty-five minutes this time and is worth knowing
about, because it looks like a RepoCortex failure and is not: Docker Desktop's
`docker-credential-desktop` helper hung indefinitely from a non-interactive
shell, so every `docker pull` blocked before it reached the daemon, and
RepoCortex's 10-minute provisioning timeout reported the pinned image as
`could not install`. The pull itself is 2.7 GB and took under ten minutes once the
helper was out of the way (`DOCKER_CONFIG` pointed at an empty config, which
makes the digest-addressed pull anonymous and changes nothing about what is
verified). The runner now asks the configured helper for `list` before every
pull, bounded at five seconds, and a helper that does not answer is reported
by name with that workaround instead of as a ten-minute `could not install`.

## Changes since the table above was measured

The resolver changed materially in one working session. The table in **Result** is
still the record for the tree it was measured on; this section records what moved
since, measured the same way, so the two are not confused with one another.

**Re-baseline before comparing.** The corpus here is a `--depth 1` prometheus clone
of **727 Go files**, not the 1,030-file checkout the table above used, and the
unmodified tree scores **86.7%** on it against the documented 86.8%. That difference
is the corpus, not a regression — which is exactly why every figure below is paired
with a baseline re-measured on the same tree and the same 57 MB `scip-go` index,
rather than compared against the published number. A −2.4pp regression measured
against the wrong baseline reads as −0.1pp of rounding.

| change | `high` precision | judged | wrong |
|---|---|---:|---:|
| baseline | 86.7% | 12,496 | 1,666 |
| + validated receiver matching, import aliases | 86.4% | 12,628 | 1,719 |
| **+ anchored import matching** | **90.2%** | 12,236 | **1,205** |

**Anchored import matching is the whole gain**, +3.8pp on its own. It replaced the
bag-of-words segment test in `importSet.covers`, whose doc comment had argued the
opposite: *"Deliberately generous… being too strict here would silently drop real
edges, which is the more expensive mistake."* Measurement disagrees. Generosity was
not free: a loose match that happens to be unique returns `ConfidenceHigh`, so
looseness manufactures confident wrong edges. It removed roughly 4,000 edges overall
and 514 wrong ones from the judged set at a cost of 392 judged-correct.

**Validated receiver matching is neutral here, which is not the same as disproven.**
It only bites where a fully-qualified name carries its owner — Java, Kotlin,
TypeScript. Prometheus is Go, where `model.DefaultFQN` synthesises `path:name` FQNs
with no owner, so the channel it tightens barely fires. It is unmeasured on the
languages it exists for.

### The LanguageSpec refactor: proven by digest, not by percentage

Per-language behaviour used to live in three separate switches on language name
inside `resolver.go` — `packageIsDirectory` (go/java/kotlin), `receiverIsAType`
(java/kotlin/typescript) and `memberSeparators` (java/kotlin/scala | ruby/rust/
cpp/c/php). Three switches, three different language sets, no single place that
said what a language does. `LanguageSpec` replaces them with one declarative
per-language table, selected by a factory held per-`Resolver`.

A refactor of the resolver has to prove it changed nothing, and **precision was
the wrong instrument for that proof here.** RepoCortex's own source *is* the
corpus, so adding two files and shifting line numbers underneath a `scip-go`
index built before the move changes which lines are judgeable at all. That alone
read as 97.8% → 97.6% — a number that looks like a small regression and is
actually an artifact of measuring a moving corpus.

So the check was a controlled A/B on a **frozen** copy: identical tree, identical
SCIP index, only the binary differing. The **post-resolve registry digests were
identical**:

```
a1ebd89167c5ad84916f4f6920a313edcaf83b1e8778201cc75e2f404413903d
```

with precision unchanged at `high` 97.8%, 4,631 judged, 102 disagree.

That is a strictly stronger claim than matching percentages. Equal precision means
the same *number* of edges were right; an equal digest means the resolved graphs
are **byte-identical** — same edges, same tiers, same evidence, same order. Two
different graphs can score the same and differ everywhere, which is precisely
what a percentage cannot rule out and a digest can.

**Determinism re-checked at the same time**: one index digest and one
post-resolve digest across 10 runs at each of 1, 2, 4, 8 and 16 workers.

```
index         9c39d5dc54072878df9e49b904105237ba100fd3feb496c597f43433b0ce1904
post-resolve  a1ebd89167c5ad84916f4f6920a313edcaf83b1e8778201cc75e2f404413903d
```

The general rule worth taking from this: **when refactoring something whose
output is reproducible, compare the output, not a score computed from it** — and
freeze the corpus first, or the measurement moves under the thing being measured.

### The cap-drop counter, and what it immediately found

`MaxCandidates` bounds fan-out, and dropped candidates were folded into
`Ambiguous`. So "this name is too common to weigh" and "two candidates and no
evidence to choose" were the same number. That mattered more once
`repocortex accuracy` began publishing recall, because this was a named blind spot
inside it.

Counters only — nothing in the binding path reads them — and the proof is that
the registry digest is **byte-identical** to the arm without them
(`8c90385d…`), which is stronger than equal percentages.

**On this repository: 1,093 cap drops, 47.3% of the 2,311 ambiguous references.**
Nearly half of "ambiguous" was never a disambiguation problem.

The counter's first act was to show that a feature is inert. `narrowByReceiver`
exists to rescue exactly these over-subscribed names, and it is gated on
`ReceiverDenotesType`, which is false for Go — so **it rescued 0 of 1,093** and
cannot fire on a Go repository at all.

### Common-name suppression: it ships, and it is smaller than it sounds

| tier | baseline | with suppression |
|---|---|---:|
| `high` | 98.0%, 5,475 judged, 108 wrong | **identical** |
| `high` recall | 60.4% | 60.4% |
| `low` | 0.3%, 323 judged, 322 wrong | 0.3%, 296 judged, 295 wrong |

It moves `high` **not at all** — the three numbers are identical, not merely
close. It removes 78 `low` edges, of which the 27 adjudicable ones were 27 wrong
and 0 right, and recall is unchanged at every tier.

Smaller still: `impact.DefaultMinConfidence` is `medium`, so a default consumer
**already never saw these edges**. The win is invisible unless someone passes
`--min-confidence low`. Shipped because it is strictly non-negative and removes
noise, not because it improves a number anyone acts on. Recording that
distinction matters more than the change does.

### A third change that measured badly and was reverted — receiver type inference

A per-file local type environment, so a Go receiver arrives as `Service` rather
than as `s`. The parser half worked exactly as designed — `svc *Service` →
`Service`, `c := &Client{}` → `Client`, and `n := NewService()` correctly left
alone, because recovering a type from a `New` prefix is a convention, not a
declaration.

| arm | `high` prec | `high` recall | `medium` prec | all recall |
|---|---:|---:|---:|---:|
| baseline | **98.6%** | 60.4% | 35.4% | 60.6% |
| type env only | 98.3% | 60.2% | 50.0% | 60.5% |
| + receiver-denotes-type | 98.3% | 60.6% | **54.5%** | **61.0%** |

The last row is the tempting one: `medium` nearly doubles, all-tiers recall
gains 0.4pp, and the cap rescue finally fires. But in `high` it added **18
correct edges and 14 wrong ones — a 56% hit rate into a tier measuring 98.6%.**
That is the A19 mechanism one tier up: dilution of the tier the product tells you
to act on. Reverted.

**The mechanism is reusable and is the point of recording this.** Tier 2
(receiver) sits *above* tiers 3 and 4 (import, same-directory). Go is a
directory-is-package language where proximity is strong evidence — and while Go
receivers arrived as `s` and `c`, tier 2 almost never fired, so proximity
effectively decided. Supplying real type names woke tier 2 up, and a file-stem
coincidence (`Store` matching `store.go`) began out-ranking the correct
in-package definition.

So this is not "Go type inference does not work". It is **"the tier order was
tuned for a receiver that carried no type."** For a directory-is-package
language, inferred type evidence must not outrank package proximity. Retrying
means moving tier 2 below `pickSameDir` where `PackageIsDirectory` holds, and
measuring that.

**One safety property does not hold as stated.** "A mis-inference yields no
edge, never a wrong one" is true for `high` — `receiverMatches` validates rather
than contains, and tier 2 reports High only on a unique survivor. It is false in
general: a receiver matching *two* candidates by file stem takes tier 2's
multi-candidate branch and emits a deterministic preference at `medium`. A test
now pins both halves, so closing that gap is deliberate and widening it fails.

### Two changes that measured badly and were reverted

Both were adopted from a competitor's resolver, both looked plainly safe, and
neither survived measurement. They are recorded because the reasoning that produced
them will recur.

**A bare call cannot name a method in Go or Rust.** True of the languages, false of
our parser. `ref.Receiver == ""` means the receiver query could not express the
receiver, not that the source wrote none — `receiverQueries` captures only a simple
identifier receiver, so `s.repo.Save(id)` arrives with an empty receiver and every
chained call in Go was read as bare. `repocortex callers Save` went from two bound
callers to zero. Every unit test passed throughout, because they set `Receiver`
directly; only the end-to-end CLI path exercised the parser that leaves it empty.

**Production code never binds to a test definition.** Cost **2.4pp** of `high`
precision. It does not merely delete wrong test-file candidates: by shrinking the
candidate set it tips references across the disambiguation thresholds, so references
previously recorded as *ambiguous* now resolve — 426 of them, of which 64 were right
and **362 wrong**. It converts honest ambiguity into confident wrong answers, which
is precisely what `internal/resolve/infer`'s package doc forbids.

The transferable rule, now stated in the resolver: **a filter that removes candidates
is not automatically safe, because removing candidates also removes the ambiguity
that was protecting the caller.** "Fewer candidates" and "fewer wrong edges" are
different claims, and only the second is worth having.

### A4 — arity disambiguation, which ships

When several candidates share a name, the number of arguments at the call site
is evidence about which one was meant. `Save(id)` and `Save(ctx, id, opts)` are
different functions. Until this change the database could not hold that fact:
`symbols.signature` existed and `internal/parse` never populated it for code
symbols, and `symbol_references` had no notion of an argument list at all.

Migration `0010_arity` adds `symbols.param_count` and
`symbol_references.arg_count`, and the count is plumbed parse → pipeline →
registry → resolver.

**It is a RANKING change, not a filter, and that is the whole design.** It is
consulted at exactly three points, every one of them a branch where the resolver
has ALREADY chosen a candidate and is about to report `medium` with the reason
"several matched, preferred deterministically". All it changes is WHICH of those
several is preferred. It cannot remove a candidate, cannot resolve a reference
that would otherwise have been ambiguous, and cannot reach a `high` branch —
every one of those returns before arity is consulted. That is what makes it a
different shape of change from A19, which lost 2.4pp of `high` precisely by
shrinking candidate sets until honest ambiguity tipped into confident wrong
answers.

Measured on RepoCortex's own source, 431 Go files, against an 18 MB `scip-go`
0.2.7 index. One **frozen** tree and one index in both arms, only the binary
differing:

| tier | baseline | with A4 | judged | wrong: baseline → A4 |
|---|---:|---:|---:|---:|
| `high` | 97.8% | **97.8%** | 5,747 | 129 → **129** |
| `medium` | 35.4% | **45.8%** | 48 | 31 → **26** |
| `low` | 0.3% | 0.3% | 309 | 308 → 308 |
| all | 92.3% | 92.4% | 6,104 | 468 → 463 |

`high` is **identical in all three numbers**, not merely close — which is what
the design predicts rather than a result it got lucky with. Recall is unchanged
at every tier (`high` 60.3%, all-tiers 60.5%), and so are the miss counts:
2,891 unresolved and 407 ambiguous in both arms. The same call sites resolved,
at the same tiers, and five of them changed which definition they name. **All
five changed from wrong to right.**

Determinism re-checked at the same time: one index digest and one post-resolve
digest across ten runs at each of 1, 2, 4, 8 and 16 workers.

```
index         d30573e74d317782be62fb613f781e38b0507dc68d093b9541c6e7bf5e5217cd
post-resolve  d6be8fcf1b179f0fe107435e1e494b3b1004db80344ee2cbd4832837c1adbc8d
```

The index digest is byte-identical to the baseline arm's, which is the expected
result and worth stating: `param_count` is an INPUT to resolution and the digest
covers the graph, so indexing is unmoved and only the resolved edges differ.

**Two things this does not claim.**

*The sample is small.* `medium` is 48 judged edges out of the 13,394 heuristic
ones on this corpus, so 10.4pp is five edges. What justifies shipping is not the
size of that number but the shape of the change: it cannot subtract, and every
edge it moved it moved in the right direction. A corpus with a larger `medium`
tier would measure it better, and prometheus is the obvious next one.

*Only Go is enabled.* Counting is language-specific in ways that are easy to get
subtly wrong, and a wrong arity is worse than none because it is evidence
pointing at the wrong candidate. Go declares several parameters in one node —
`func f(a, b int)` is ONE `parameter_declaration` with two `name:` fields, so
counting nodes reports 1 for a function of arity 2. Python writes `self` at the
declaration and not at the call site. TypeScript's optional parameters mean a
declared count of three is satisfied by a call passing one. Java is the language
overload resolution matters most in and the one language here that had no
compiler ground truth for until spring-petclinic-rest. Variadic forms are
refused outright in every language, on both sides, because an open-ended
signature cannot be compared to a call at all.

The encoding carries that refusal. `pkg/model.Arity` and the SQL columns are
offset by one, so **zero means UNKNOWN and a count of n is stored as n+1**. A
plain integer cannot express the difference between "this call passes no
arguments" and "nobody worked out how many arguments this call passes", and
those are opposite facts — the first narrows candidates, the second is the
absence of evidence. Conflating them would let every pre-migration row, every
SCIP-imported symbol and every language without an arity query assert that it is
niladic. That is A18's mistake one field over: reading "the parser could not
express it" as "the source did not write it".

### A4 on TypeScript

The A4 section above ends with the rule that a language ships arity when a
measurement against a compiler index says it helps. This is that measurement
for TypeScript, and the answer was no — not because the count was wrong, but
because it moved nothing.

**The count that was tried.** TypeScript counts ONLY when every parameter is
required. An `optional_parameter` (`b?: number`) or a `spread_element`
(`f(...xs)`) makes the whole list unknown by node type; a `required_parameter`
makes it unknown when it carries a `value:` field (`b = 1`) or its `pattern:`
is a `rest_pattern` (`...xs: number[]`). That is the only honest count, because
the resolver compares by equality and a declared count several call shapes
satisfy is not a count. The unit tests for those five shapes passed, and on
nest the rule produced a known parameter count for 3,061 symbols and a known
argument count for 39,959 references, so the numbers below are a test of the
idea rather than of the plumbing.

**Measured on nest** (`nestjs/nest` at `39fbddae51281ca56bf2fed9123d98e61509066c`,
`--depth 1`), against the root `scip-typescript` 0.3.14 index (9.2 MB, produced
by `repocortex indexers install --yes` without `node_modules`, so third-party
imports do not resolve and the index is smaller than a full build would give).
One tree and one index in both arms, only the binary differing.

Before:

| tier | sites | right | wrong | precision | recall | F1 |
|---|---:|---:|---:|---:|---:|---:|
| `high` | 1,004 | 962 | 42 | 95.8% | 37.5% | 54.0% |
| `medium` | 181 | 160 | 21 | 88.4% | 6.2% | 11.7% |
| `low` | 38 | 7 | 31 | 18.4% | 0.3% | 0.5% |
| all | 1,223 | 1,129 | 94 | 92.3% | 44.1% | 59.7% |
| missed | 1,339 | — | — | — | — | — |

After:

| tier | sites | right | wrong | precision | recall | F1 |
|---|---:|---:|---:|---:|---:|---:|
| `high` | 1,004 | 962 | 42 | 95.8% | 37.5% | 54.0% |
| `medium` | 181 | 160 | 21 | 88.4% | 6.2% | 11.7% |
| `low` | 38 | 7 | 31 | 18.4% | 0.3% | 0.5% |
| all | 1,223 | 1,129 | 94 | 92.3% | 44.1% | 59.7% |
| missed | 1,339 | — | — | — | — | — |

2,562 adjudicated call sites in both arms; 218 unresolved, 1,116 ambiguous and
5 attributed to no caller in both. The two `accuracy --json` outputs are
byte-identical, and so are the two CALLS edge sets: 3,000 edges in each arm
(1,744 `exact`, 540 `high`, 205 `medium`, 511 `low`), and not one differs in
source, target, tier or evidence line. Arity did not move a judged edge, and
it did not move an unjudged one either.

**Decision: it did not ship.** The gate was that `high` precision must not
drop by more than 0.2pp AND either recall or the `high` edge count must rise.
Precision held exactly, but nothing rose, and a change that measurably does
nothing is not a change to carry — every arity query is a grammar-shaped
contract that has to survive every future grammar bump, and the tier table
would be promising `typed` for a discrimination that has never discriminated.
`internal/parse` was reverted; TypeScript stays `resolved`.

Why it moved nothing is worth recording, because it is not the reason the A4
section predicted. That section worried that optional parameters would make
the count WRONG. They did not — the rule refuses those. The reason is where
the count is consulted. `preferByArity` runs at three points, each a branch
where SEVERAL candidates survived a receiver match, an import's local name, or
an explicit import, and one has to be preferred. On nest, TypeScript reaches
those branches almost never: 204 of the 205 heuristic `medium` edges carry the
detail "same directory, but the language requires an import", which is the
branch that fires when EXACTLY ONE same-named neighbour exists — a branch with
nothing to choose between, and which does not consult arity for that reason.
The one remaining `medium` edge is "multiple matches among imports", and its
candidates did not separate by count. So the counts were there (a known
argument count sat under 202 of the 205 `medium` sites) and the rule was
correct, and it was asked exactly once. Meanwhile 1,116 adjudicated sites are
ambiguous with no edge claimed at all, and arity cannot reach those by design:
it prefers among candidates the resolver has already accepted and never
removes one, which is the property that kept `high` unmoved. The ceiling on
TypeScript recall here is candidate selection, not candidate ranking, and the
evidence that would resolve those sites is a receiver type rather than an
argument count.

## A determinism defect the digest caught

Measured on RepoCortex's own source (425 files, `scip-go` 0.2.7 as ground truth):
`high` **97.8%** over 4,619 judged edges, `medium` 34.1%, `low` 0.0%, all tiers
92.7%. Coverage: 4,890 of 10,484 heuristic `CALLS` edges adjudicated against
22,389 compiler facts on the 12,313 lines the compiler resolved to exactly one
target.

Running that measurement surfaced something the precision numbers do not show.
The registry digest **disagreed between runs over byte-identical source**, and
across worker counts — three distinct digests across six repeats.

Indexing was not at fault: index-only runs were identical 4 of 4. `resolve` was,
and it reproduced with the deferred inherited-dispatch pass disabled, so it was
not new work. `repocortex diff` between two divergent runs named the cause exactly:

```
orphaned migrations/0003_data_access.sql:table:table_accesses  inbound 14 -> 0
removed  verifyGraph -[READS]-> 0003_data_access.sql:table:table_accesses
added    verifyGraph -[READS]-> 0005_fk_indexes.sql:table:table_accesses
```

`table_accesses` is created in `0003_data_access.sql` and named again by a
`CREATE INDEX` in `0005_fk_indexes.sql`, so two symbols carry the name.
`tableSymbols` picked the canonical one by "first declaration wins" over a
`SELECT` with **no `ORDER BY`** — and an unordered `SELECT` has no first. Row
order followed the query plan over ids the *parallel* indexing pass assigned, so
the winner changed between runs, moving 40+ `READS`/`WRITES` edges and orphaning
two table nodes.

Ordering by path then line makes "first" mean first in the migration sequence,
which is where the `CREATE TABLE` is — so the canonical node became the correct
one as well as a stable one. After the fix: identical digests across six repeats
and across 1, 2, 4, 8 and 16 workers.

Two things are worth keeping from this. **A digest that is only checked in CI on
small fixtures will not catch an ordering bug that needs a name collision to
appear** — this one needed a real repository with a real migration history.
And **`ORDER BY` is a correctness clause whenever a query feeds a first-wins
rule**, not a formatting preference; the comment above the loop already said
"first declaration wins" while the query left "first" undefined.

## Inheritance for Ruby, C#, Rust and C++: measured, 12 November 2026

Before this change these four languages recorded no `EXTENDS` or `IMPLEMENTS`
edge at all. `find_implementations` answered "none declared", and a method
inherited from a base class could not be bound when it was called on a subclass.
Go, Java, TypeScript and Python already had it. This section is what the
supertype queries for the other four capture, what that did on one public
project each, and where it is wrong.

The resolver note that said these languages needed "no resolver change" is true
as far as it goes: `inherit()` parks `self`, `super` and typed calls for the
ancestry pass, and with supertype references present they bind (a fixture in
`fixtures/receivers` pins `self.m`, `this.m`, `base.m` and a bare inherited call
in Ruby, C# and C++). Two things it does not say. The hierarchy is kept **by
type name across the whole index, whatever the language**, so two types of one
name make every call through either of them ambiguous (honest, not wrong; the
Ruby fixture pins it). And C# `this.m()` and `base.m()` were not extracted as
calls at all, because the call pattern wants an identifier on each side and
`this` is a keyword token. That is fixed in the C# entry of `queries.go`.

### What is captured

| Language | Captured | Edge kind | Not captured |
|---|---|---|---|
| Ruby | `class A < B`, `class A < Mod::B` | `EXTENDS` | `class A < Struct.new(..)` and `Class.new(B)` (built at run time); `class Foo::Bar` has no class symbol, so its `<` has no source |
| Ruby | `include M`, `prepend M`, `extend M` (constants, plain or `A::M`, several per call) | `IMPLEMENTS` | `include` on a receiver, or of a non-constant |
| C# | `class A : B, IFoo`, generics `B<T>`, qualified `Ns.B` | `IMPLEMENTS` if the name reads `I` + capital + lower-case, else `EXTENDS` | an enum's underlying type; a base list with a `#if` inside it keeps the directive in the name and binds nothing |
| C# | `struct S : IFoo` | `IMPLEMENTS` (a struct can only implement) | |
| C# | `interface I : IBase` | `EXTENDS` | |
| C# | `record R(..) : Base(..), IFoo` | `Base(..)` is `EXTENDS`; the rest as a class | |
| Rust | `trait A: B + C`, `ns::B<T>` | `EXTENDS` | standard-library traits (`Debug`, `Send`, `Clone`, ...): see below; **`impl Trait for Type`**: see below |
| C++ | `class A : public B, private C`, `struct S : B`, `Base<T>`, `ns::B`, up to three levels of qualifier | `EXTENDS` (C++ has no interface construct) | `Base<T>::type`, a member of a dependent type |

Mixins are `IMPLEMENTS` for the reason Java's interfaces and Rust's traits are:
the module contributes methods without being the parent. `extend M` also lands
there although it adds *class-level* methods; this graph cannot tell the two
apart, so a call that only an extended module defines can bind from an instance
method too. The ancestry pass only runs for a call pass one refused, which
bounds it.

C# does not say in the syntax which entry of a base list is the class. The
`I` + capital + lower-case convention decides it for classes and records. On
Newtonsoft.Json 274 of 275 edges carry the label the target's kind implies (one
class is recorded as `EXTENDS` an interface). Both kinds are consulted together
when a call is bound through the ancestry, so the label changes the edge kind
and not the answer.

### What was measured

Shallow clones of the default branch on 12 November 2026; `init`, `index`,
`resolve`, binary built before and after.

| Project | Supertype references | `EXTENDS` / `IMPLEMENTS` edges, before | after (`high` / `medium` / `low`) |
|---|---|---|---|
| sinatra (Ruby) | 138 (106 + 32) | 0 / 0 | 17 (13 / 0 / 4) and 17 (14 / 0 / 3) |
| Newtonsoft.Json (C#) | 872 (724 + 148) | 0 / 0 | 206 (112 / 88 / 6) and 69 (45 / 20 / 4) |
| ripgrep (Rust) | 0 (8 before the standard-library filter) | 0 / 0 | 0 / 0 |
| leveldb (C++) | 75 | 0 / 0 | 24 (6 / 15 / 3) and 0 |

Most references that do not become an edge name a type outside the repository
(`Rack::Request`, `StandardError`, `IEnumerable`, `std::..`): sinatra 138
references, 34 edges.

Precision: edges drawn at random (seed 42, Python's `random.sample` over the
edges ordered by id; all of them when there are fewer than 30), each judged by
reading the declaration and the target.

| Project | Judged | Right | Wrong | Note |
|---|---|---|---|---|
| sinatra | 30 of 34 | 29 | 1 | `class Request < Rack::Request` bound, at `low`, to `Sinatra::WebDAV::Request`: the qualifier is not carried, and `Rack::Request` is outside the repository. Two `extend` edges have a method (`Namespace.new`) as their source because the `extend` is inside a `Module.new do` block; the target is the declared module |
| Newtonsoft.Json | 30 of 275 | 30 | 0 | |
| ripgrep | 1 of 1 | 0 | 1 | `trait Flag: Debug` bound to a struct named `Debug` in the next file of the directory. This is why the standard-library traits are no longer captured; after the filter there is no Rust edge to judge |
| leveldb | 24 of 24 | 10 | 8 | 6 more are right only as a **forward declaration** of the right type (`class Cache;`), which is the same type and not its definition. See below |

Recall: the five largest non-test source files by line count, every declared
supertype relationship whose target is defined in the repository, listed by hand
from the source.

| Project | Files | Listed | With the right edge |
|---|---|---|---|
| sinatra | `lib/sinatra/base.rb`, `sinatra-contrib/lib/sinatra/{reloader,namespace,cookies}.rb`, `lib/sinatra/show_exceptions.rb` | 9 | 9 |
| Newtonsoft.Json | `Utilities/LinqBridge.cs`, `Linq/JToken.cs`, `Serialization/JsonSerializerInternalReader.cs`, `JsonTextReader.cs`, `Converters/XmlNodeConverter.cs` | 36 | 33 |
| ripgrep | `crates/core/flags/defs.rs`, `crates/printer/src/standard.rs`, `crates/ignore/src/walk.rs`, `crates/globset/src/glob.rs`, `crates/ignore/src/dir.rs` | not counted: the relationship is `impl Trait for Type` | 0 |
| leveldb | `db/db_impl.cc`, `db/version_set.cc`, `benchmarks/db_bench.cc`, `util/env_posix.cc`, `util/env_windows.cc` | 16 | 2 |

The three C# misses: `XmlNodeConverter : JsonConverter` and `JsonTextReader :
JsonReader` are ambiguous (`JsonConverter` and `JsonConverter<T>` share a name in
one file, `JsonReader` is a partial class in two files), and `JToken`'s
`IJsonLineInfo` carries a preprocessor directive in its name.

### What the effect on call resolution was

| Project | `CALLS` `high`, before to after | `medium` | `low` |
|---|---|---|---|
| sinatra | 1616 to 1620 | 10 to 10 | 671 to 668 |
| Newtonsoft.Json | 7227 to 7306 | 2725 to 2731 | 4149 to 4143 |
| ripgrep | 3134 to 3134 | 870 to 870 | 411 to 411 |
| leveldb | 1442 to 1442 | 78 to 78 | 508 to 508 |

sinatra: 1 new edge, 3 moved from `low` to `high`; all four right by reading
(`Application.run!` to `Base.run!`, and three `ContentFor` calls to
`EngineTracking.erb?` two levels up through `Capture`). Newtonsoft.Json: 79 new
edges and 22 moved from `low` to `high`. Of the new, 47 come from the ancestry
(all `high`) and 32 from the `this.` and `base.` calls that are now extracted
at all (10 `high`, 6 `medium`, 16 `low`). A random 20 of the 79 (seed 42): 17
right, 3 wrong. The three wrong are `low` edges from `this.PropertyChanging(..)`
in a LINQ-to-SQL designer file, an event being invoked, bound to a method of that
name in an unrelated test class. All 14 `high` and the `medium` one in the sample
are right, and the 22 that moved were read as well (`IsErrorHandled`,
`ClearErrorContext`, `GetTokenIndex` up one `JsonSerializerInternalBase` or
`PathFilter`; right). No tier is demoted: `high` is 14 of 14 here and stays
above its floor of 86.8%, and the wrong ones are at `low`.

The `high` an inherited call earns comes from the Python measurement above
(99.1% over 7,575 bindings). These four languages have a different cost to being
wrong, and this is a sample of 14 on one of them, so it is a floor to keep
watching, not a measurement of its own.

ripgrep and leveldb did not move because their calls are not the kind inheritance
helps: Rust has no edges (below), and leveldb's subclass calls are on `this->`
into a base class that is not indexed (below).

`implementations` before and after, first lines:

```text
sinatra   Error                  before: none declared
                                 after:  extends BadRequest base.rb:277 high; extends NotFound base.rb:281 high
sinatra   Capture                before: none declared
                                 after:  implemented by ContentFor, HamlHelpers (high); implements EngineTracking (high)
Newtonsoft IJsonLineInfo         before: none declared
                                 after:  implements JsonTextReader (medium), JsonValidatingReader (medium), JTokenReader (low), TraceJsonReader (low)
Newtonsoft DefaultContractResolver  before: none declared
                                 after:  25 subclasses, e.g. LowercaseContractResolver, ShouldSerializeContractResolver (high)
ripgrep   Flag, Sink             before: none declared;  after: none declared
leveldb   Constructor            before: none declared
                                 after:  extends BlockConstructor, TableConstructor, MemTableConstructor, DBConstructor (high)
leveldb   Comparator             before: none declared
                                 after:  4 "subclasses" (medium), all bound to the wrong Comparator, below
```

### Where it is wrong or missing

**Rust `impl Trait for Type` is not recorded, and that is most of Rust's
hierarchy** (ripgrep has 247 of them and three supertrait lists, all standard
library). The reference would sit on the `impl` block, which is not a symbol,
and the resolver gives an edge the innermost symbol on the reference's line as
its source, so there is none. Doing it needs two changes outside the files this
work was allowed to touch: the parser would carry the implementing type with
the reference, and `bind()` in `resolver.go` would take that type, not the
enclosing symbol, as the source. The methods in `impl Type { }` and `impl Trait
for Type { }` are already owned by the type (`shape.go`), so `self.m()` in them
binds; what is missing is only the type's link to the trait.

**C++ precision is the weakest of the four.** The 8 wrong edges in leveldb all
have one of three causes. `class LEVELDB_EXPORT Comparator { ... }`, the form of
every public class in the library, does not parse as a class, so `Comparator`,
`Iterator`, `Env`, `Cache` and the rest are not symbols, and the subclass binds
to whatever else carries the name: a `struct Comparator` local to
`skiplist_test.cc` (4 edges), a `class Handler` local to a function in
`db_test.cc` (3, from `WriteBatch::Handler`, whose qualifier is not carried), and
a `typedef Iterator* (*BlockFunction)(..)` that the grammar's own tags read as a
declaration of `Iterator` (1, at `high`, "defined in the same file"). Forward
declarations (`class Cache;`) are symbols too, and 6 edges end at one. The
same-directory rule gives these a `medium` that the call rules would give `low`
(`Neighbour` in `calltraits.go` is 6 of 12 for C++); it is applied in
`pickSameDir`, which supertype references take and calls through `shaped.go` do
not. The `medium` hierarchy edges in leveldb are 8 right of 15, below the
tier's floor of 60%. Changing that tier is a change to `resolver.go`, which this
work could not make, so the number is reported and the tier stays.

Recall on leveldb is 2 of 16 for the same reason: the targets (`Env`,
`SequentialFile`, `Iterator`, `Comparator`) are macro-exported classes that are
not indexed.

**Ruby** misses what the grammar does not call a call: a bare `breathe` (an
identifier with no receiver or arguments) is not extracted, and neither is
`super`, so a subclass reaching an inherited method that way gets no call edge.
**C++** does not extract `Base::method()`, the explicit base call.

### Tests

`internal/parse/inheritance_ruby_csharp_rust_cpp_test.go` pins what each query
captures, what it must not (plain files, `Struct.new` bases, enum underlying
types, `where Self: B`, dependent-type bases), that one clause gives one
reference per type, and the C# `this`/`base` calls.
`TestInheritanceQueriesCompileForEveryLanguage` now checks that the
inheritance query is still in the compiled tags query, where it used to accept
any `@reference.` capture and so could not see the query being dropped.
`fixtures/receivers` gains `rb/lib/zoo.rb`, `rb/vendor/animal.rb`, `cs/`,
`cpp/zoo.cc`, `cpp/far/`, `rs/src/shapes.rs`, and the marker grammar gains
`super X -> ID` (a hierarchy edge to that definition, and to nothing else) and
`nosuper X`.

## JavaScript, TypeScript and TSX: CommonJS, JSX, wrapped components (13 November 2026)

A hand check on real projects found that most call sites in these three
languages ended in no edge: 12.5% on express (CommonJS), 34% on a React TSX
project, 44% on zod (56% after the earlier resolver fix). This section is what
changed, measured on `expressjs/express` (`43fca52d`), `colinhacks/zod`
(`19c40d20`) and `shadcn-ui/taxonomy` (`298a8857`, a Next.js application with
131 files indexed as JavaScript, TypeScript or TSX), `--depth 1` clones, each indexed
and resolved by a binary built before the change and one built after it.

The reviewer of every sample below is the AI assistant that wrote the change,
reading the caller's source and the target, strictly: a call bound to a
same-named member of another object is wrong. It is not the independent
sub-agent review the sections above used, and the per-tier samples are 30 edges
or all of them; read the intervals accordingly.

### What is extracted and bound now

| | Before | After | Example |
|---|---|---|---|
| `exports.f = function`, `module.exports.f = ...` | not a definition | function `f` | express `lib/utils.js`: `exports.setCharset = function setCharset(...)` |
| `module.exports = function createApp() {}`; anonymous, named after the file | not a definition | function `createApp`; `merge-options.js` is `mergeOptions`, `widget/index.js` is `widget` | |
| `module.exports = { a, b() {}, c: function () {} }` | `b` only | `b` (method), `c` (function) | |
| `Foo.prototype.bar = function` | not a definition | method `bar` owned by `Foo`; `function Foo` is a class | express `lib/view.js`: `View.prototype.lookup` |
| `obj.name = function` at the top of a file, on an object the file declares | not a definition | method `name` owned by `obj` | express `res.send = function send(body)`, `app.use = ...` |
| `const x = require('./x')`, `{ a, b: c } = require(...)`, `require('./x').y` | no import row | module alias `x`, members `.a` `.c`, `.y` | express `var setCharset = require('./utils').setCharset` |
| `exports.f()` / `module.exports.f()` inside the module | unbound | the module's own `f` | express `utils.js` |
| `new Foo()` | no reference | a call on `Foo`, bound as `INSTANTIATES` | zod `new core.$ZodAsyncError()` |
| `<Button />`, `<Foo.Bar />` | no reference | a call on `Button`, shaped like `Button()`; `CALLS` to a function component, `INSTANTIATES` to a class component | taxonomy `<DashboardHeader />` |
| `<Ctx.Provider>`, `<Ctx.Consumer>` | no reference | a `reference.jsx` to `Ctx` (`REFERENCES`) | |
| `const X = forwardRef(...)`, `memo`, `lazy`, `observer`, `defineComponent`, `styled.x\`...\``, `styled(Y)\`...\`` | no symbol, or none for a call-valued constant | function | taxonomy `const Menubar = React.forwardRef(...)` |
| `createContext(...)` | no symbol | constant | |
| `export const X = value`, `export let/var` | no symbol | constant / variable; also when an `export { X }` clause lists a top-level `const X = call()` | zod `export const $ZodType = core.$constructor(...)` |
| `PostItem.Skeleton = function`, `<PostItem.Skeleton />` | unbound | method `Skeleton` owned by `PostItem`, bound through the import | taxonomy `PostItem.Skeleton` (5 uses) |

**JSX is a call.** A function component is a function React calls, and a class
component is one React constructs, which is exactly the split between `CALLS`
and `INSTANTIATES` that a call bound to a function or to a class already makes.
So the tag is recorded the way a call is, from the element's name (a bare
`<Button />` is a bare call, `<Foo.Bar />` is a call of `Bar` on `Foo`), and
every rule that binds a call binds it, including the import-exact ones.
`find_callers` on a function component lists where it is rendered:
`repocortex callers DashboardHeader` on taxonomy lists six callers, where it
listed none before. Only capitalised names are components; `<div>` records
nothing. A context's `.Provider` and `.Consumer` are parts of the context
object, so those two record a plain reference to the object instead.

The limits, because each is a decision:

- `exports.x = ...` and `module.exports = ...` define only as a statement at
  the top of the file, or as the value of a top-level `var x = ` chain
  (`var app = exports = module.exports = {}`).
- `obj.name = function` defines only as a top-level statement on an object the
  same file declares at its top level. Inside a function, on `this`, on an
  undeclared object, or assigning a name (`exports.alias = exports.parse`), it
  defines nothing.
- `Foo.prototype.bar = function` defines wherever it is written.
- A non-exported `const x = 5` or `const y = compute()` is not a definition. The
  inferred function patterns are untouched.
- A renamed require or import (`{ b: c }`) binds under the local name `c`, so
  `c()` looks for a definition named `c`, not `b`.
- `require` is read at the top of a file only. A `require` inside a function
  records its path and binds no name.
- `require('..')` and `require('./')` name a directory, and the resolver's
  import table drops a path with no name in it, so the binding row names the
  directory's `index`. A test that required the package under test through
  `'..'` had bound `express.json(...)` by name alone, to `res.json`.

### What the change touched outside the language files

Everything above is in `lang_javascript.go`, `lang_typescript.go`,
`lang_tsx.go`, the script entries of `queries.go` and `import_bindings.go`, and
the JavaScript, TypeScript and TSX rows of `calltraits.go`. Four things could
not be said there, and each is the smallest edit that says it:

| File | Edit | Why it could not be said otherwise |
|---|---|---|
| `parse/generic.go` | one hook, `refineDefinitions`, called beside the shape walk and applied to each definition tag | a query cannot see the file's path (an anonymous `module.exports` is named after the file), cannot tell whether `app` is declared at the top of the file, and cannot write a qualified name (`Foo.bar`), which is how an owner is carried |
| `infer/shaped.go` | a name bound by `require` arrives as a declared value and is read as the import; `exports.f()` and `module.exports.f()` bind to the file's own `f`; a required module called by its name binds as a member of that module; `this.f()` in an ES5 constructor reads `Shape.prototype.f`; `Foo.Bar` binds a member assigned onto an imported function | each is a construct of this module system that the shared shape rules have no word for |
| `infer/calltraits.go` | `CallConstructs` (a class is reached by `new X()` and `<X />`) and `ValuesAreCallable` (a constant may hold a function: `export const parse = _parse(Err)`) for the three rows | the guard `canBind` had to be told |
| `cli/supplychain_test.go` | the expectation that a top-level `require` arrives as a call whose argument is read back from the text | it arrives as an import now, so the site no longer carries a `require` note |

`parse/shape.go` was not edited.

### Counts

References extracted, and what the resolver did with them. "Edges" are `CALLS`
and `INSTANTIATES` from source code, by tier.

| Project | Binary | Symbols | References | Resolved | Ambiguous | Unresolved (of which external) | Edges high / medium / low |
|---|---|---:|---:|---:|---:|---:|---|
| express | before | 755 | 11,832 | 304 | 21 | 11,507 (147) | 7 / 0 / 0 |
| express | after | 844 | 12,054 | 2,180 | 1,676 | 8,198 (1,913) | 158 / 0 / 7 |
| zod | before | 6,732 | 54,321 | 7,200 | 10,581 | 36,540 (13,603) | 2,730 / 1 / 74 |
| zod | after | 7,216 | 55,720 | 7,540 | 10,597 | 37,583 (13,671) | 3,700 / 227 / 7 |
| taxonomy | before | 1,329 | 757 | 248 | 0 | 509 (277) | 122 / 0 / 0 |
| taxonomy | after | 1,510 | 1,190 | 429 | 0 | 761 (461) | 408 / 0 / 0 |

zod's 155 `INSTANTIATES` edges are in its high count; taxonomy's one is too.
Express's ambiguous count rose from 21 to 1,676 because calls on `res`, `req`
and `app` now find several members of one name (`get` is a method of all three)
and refuse to choose: they were unresolved before, for want of any candidate.

### Recall

The five largest non-test source files per project (express: `lib/response.js`,
`application.js`, `request.js`, `utils.js`, `view.js`; zod:
`v4/core/schemas.ts`, `v3/types.ts`, `v4/classic/schemas.ts`,
`v4/core/compile.ts`, `v4/mini/schemas.ts`; taxonomy: `app/(marketing)/page.tsx`,
`components/ui/menubar.tsx`, `dropdown-menu.tsx`, `context-menu.tsx`,
`app/api/og/route.tsx`, which has no call to a repository definition). In each
file the first eight call sites and JSX usages, in source order, whose target
the repository defines; fewer where the file has fewer. `this.f()` in a
`defineGetter` callback and `Object.defineProperty`-style attachments count
only where a plain definition of the target exists.

| Project | Sites | Binary | Edge to the right definition | Edge to a wrong one | No edge |
|---|---:|---|---:|---:|---:|
| express | 31 | before | 0 | 0 | 31 (11 resolved to the right definition, with no symbol to be the caller) |
| express | 31 | after | 22 | 0 | 9 (8 resolved to the right definition at the top of the file; 1 ambiguous) |
| zod | 40 | before | 25 | 0 | 15 |
| zod | 40 | after | 33 | 0 | 7 |
| taxonomy | 29 | before | 3 | 0 | 26 |
| taxonomy | 29 | after | 29 | 0 | 0 |

Taxonomy's sites are nearly all `cn(...)` and `buttonVariants(...)` inside
components built by `forwardRef`, which had no symbol to be the caller. Over
the whole project, 133 JSX usages in source of the 175 components defined in
`.tsx` files gave no edge before and 133 edges after (read against the text
with `grep`, not site by site); the 30-edge sample below read the callers.

What stays unbound in the zod sites: `en()` (an anonymous default export), four
`parse.parse(...)` calls (the module re-exports constants from another module,
and a re-export is not followed), and two `this.clone(...)` in an object
literal's methods, where `clone` has six definitions. Express: `this.get()`
in a callback passed to `defineGetter`, where `this` is not a method's.

### Precision per tier

30 edges per tier that has that many, ordered by `(id * 2654435761) mod 2^32`,
judged by reading the caller. A tier with fewer is listed whole.

| Project | Tier | Right / judged | Edges | Notes |
|---|---|---:|---:|---|
| express | high | 30 / 30 | 158 | 11 of the 30 are "receiver is named after the type that declares it", 68 of the 158 |
| express | low | 4 / 7 | 7 | the only-member rule: 1 / 4; the naming rule: 3 / 3 |
| zod | high | 30 / 30 | 3,700 | 3 of 30 `INSTANTIATES` |
| zod | medium | 27 / 30 | 227 | 22 are `Schema.init(inst, def)` bound to `$constructor.init`, the interface method; the unknown-receiver rule: 5 / 8 |
| zod | low | 7 / 7 | 7 | |
| taxonomy | high | 30 / 30 | 408 | |

The wrong ones, whole: `z.null()`, `z.number()`, `z.cidrv4()` and `z.e164()`
bound to a method of the same name on a test class and on `ZodString` (`z` is a
local object, not a module); `this._def.shape()` bound to the `shape` getter of
`ZodObject` (a function stored in `_def`, not the getter); `self.engine(...)`
and `this.engine(...)` bound to `app.engine` (a view's own property);
`obj.range(...)` bound to `req.range`. All are the unknown-receiver rule, which
is why it is not above `medium` anywhere.

### Definitions

Express: every definition in the five files listed by hand from the source
(72: the 22 `res.*`, 16 `app.*` and 8 `req.*` assignments, the `exports.*` and
`View.prototype.*` ones, and the named functions). zod and taxonomy: an
independent enumeration by regular expression of classes, interfaces, type
aliases, enums, function declarations, exported constants, names an export
clause lists, and constants bound to an arrow function, a function expression
or a wrapper, scored by (type or value, name); methods and fields are not
scored on either side. The enumeration listed 16 names after the change that
were not extracted; every one is a local `const x = (...)` that is not a
function, or a local `lazy(...)` schema, so the expected count is 1,263.

| Project | Expected | Extracted correctly, before | after | Spurious, before | after |
|---|---:|---:|---:|---:|---:|
| express | 72 | 17 | 72 | 0 | 0 |
| zod | 1,263 | 993 | 1,263 | 0 | 0 |
| taxonomy | 50 | 6 | 50 | 0 | 0 |

Scoring is by (type or value, name), so `export interface $ZodType` and
`export const $ZodType` are two expected definitions. The 270 constants zod's
five files gained are most of the difference, and many share a name with an
interface that was already extracted.

### The tiers set

| Row | Field | Value | Evidence |
|---|---|---|---|
| javascript | `NamedAfterType` | high | 33 / 33 on express (the 30 sampled when it was `medium`, and the 3 at `low`), across more than ten distinct call names; promoted because the rule passed the high floor with more than one call shape |
| typescript | `UnknownReceiver` | medium | 24 / 30 on zod (80%) while the rule was still `low`, and 5 / 8 in the medium sample above, against 7 / 12 before; the floor is 60% |
| javascript | `UnknownReceiver` | low (default) | 1 / 4 on express |
| tsx | none | defaults | no unknown-receiver or named-after-type edge in the taxonomy sample |
| typescript | `UniqueTypeName` | medium (default) | 22 / 22 in the sample, all one call shape (`X.init(inst, def)`), so not enough to promote |

No rule measured below its floor, so none was demoted. The sample above found
nothing at `high` that was wrong.

### Module-level calls

A call made outside any function resolves and then produces no edge because an
edge needs a caller symbol: `bind` in `internal/resolve/infer/resolver.go`
records the resolution and returns when `enclosing(fr.Symbols, ref.Line)` finds
no symbol. Resolved references with no enclosing symbol:

| Project | Before | After |
|---|---:|---:|
| express | 297 of 304 | 2,015 of 2,180 |
| zod | 3,742 of 7,200 | 2,952 of 7,540 |
| taxonomy | 125 of 248 | 20 of 429 |

Of the 2,015 and 2,952, 1,811 (express) and 2,217 (zod) are in test files, in
callbacks passed to `describe` and `it`. Making `export const X = factory(...)`
a symbol brought the zod figure down by 790 and the taxonomy figure by 105
(most of it, not all: the other changes move it too), because the initialiser's
calls now have a caller: that is a side effect of item 4, not a fix.

**Not implemented, and what it needs.** A synthetic module symbol per file as
the caller of every call outside a symbol would add one symbol to every
JavaScript, TypeScript and TSX file (131 on taxonomy, 522 on zod, 142 on express) and
change what a caller list contains: a module that "calls" `describe` is a
caller of a different kind than a function. It needs a kind (`package` is taken
by modules and namespaces and means a type that can be extended; a new kind
touches `pkg/model` and every consumer that switches on kind), a name and FQN
that cannot collide with a function of the file's name, a range of the whole
file so that `enclosing` picks it only when nothing narrower contains the line
(it picks the last symbol that starts before the line, so it must sort first
among symbols starting on line 1), and a decision about whether it is counted
as a symbol or as an edge source only. It would change symbol counts for the
three languages and the corpus pins for them. The brief allowed it only if it
changed neither, so it is left, with the counts above to size it.

### What is still missed or bound wrongly

- Module-level calls, above.
- A required module whose exports object owns its members: `var m =
  require('./lib/proto'); m.initThing()` is unresolved when `initThing` is
  assigned onto `var proto = module.exports = {}`. The method is owned by
  `proto`, not module-level, and the module alias reaches only module-level
  definitions. Express's own `app.*` calls bind by the receiver's name instead.
- A re-export is not followed: zod `mini/schemas.ts` calls `parse.parse(...)`
  through `mini/parse.ts`, which re-exports it from `core/parse.ts`.
- Anonymous default exports: `export default function () {}` has no name to
  bind (zod `en()`).
- Methods attached by a loop (`methods.forEach(function (m) { app[m] = ... })`
  makes express's `app.get`) and properties defined by `Object.defineProperty`.
- `this.f()` in a callback that is not a method: ambiguous when several types
  declare `f`.
- Renamed imports and requires bind under the local name.
- The unknown-receiver rule's wrong bindings above, at `medium` in TypeScript.

Vue and Svelte single-file components reuse these parsers through `sfc.go`,
which was not edited. No `.vue` or `.svelte` fixture changed what it extracts:
the corpus gate, which pins a Vue file, passes unchanged.

Reproduce with `fixtures/receivers/{js,ts,tsx}` (`TestReceiverFixtureBindsNothingWrongly`),
`fixtures/langs/{javascript,typescript,tsx}` (`TestLanguageConformance`), and the
three clones above indexed with `repocortex init`, `index` and `resolve`.

## C and C++: macros in declarations, measured, 13 November 2026

Tree-sitter parses the text as written and nothing runs the preprocessor. A
declaration behind a macro the grammar has never seen is therefore not a
declaration to it: `class LEVELDB_EXPORT Comparator { ... };` is a class named
`LEVELDB_EXPORT`, or a function called `Comparator` whose return type is a class
and which has no error node at all. Every public class of leveldb is written
that way, so `Comparator`, `Env`, `Iterator`, `Cache` and `DB` were not symbols
and every subclass bound to an unrelated type of the same name (the "C++
precision" paragraph above). This section is the change that fixes the
declarations, what it did on four C and C++ projects and on one Objective-C
file, and what it did not fix.

### What it does

`internal/parse/cmacro.go`, for C, C++ and Objective-C only. The source is
scanned as text (comments, strings and preprocessor lines skipped) for the
places an unknown macro is written in a declaration, and each one found is
overwritten with spaces of the same byte length, newlines kept, so no offset and
no line number moves. The forms:

| Form | Example |
|---|---|
| between `class` / `struct` / `union` / `enum class` and the name, when `{`, `:`, `final` or (C++) `;` follows | `class LEVELDB_EXPORT Comparator {`, `struct API(dllexport) S : B`, two macros in a row |
| after the name, before `{` or `:` | `struct node PACKED_ATTR {` |
| in front of a function declaration or definition, with its storage words, with or without arguments, when what follows is a head that ends in `(` | `JK_STATIC_INLINE void f(`, `static REDIS_NO_SANITIZE("bounds") int f(`, `PY_LOCAL(int) f(`, `LEVELDB_EXPORT Status Destroy(` |
| in the middle of a head, after its first word and before more of it | `void JEMALLOC_NOTHROW *JEMALLOC_ATTR(malloc) je_malloc(` |
| after the parameter list of a function (not of `if`, `for`, `while`) | `void Compact() LOCKS_EXCLUDED(mu_) {` |
| a single argument-less macro alone on its line in front of the next declaration, directive, `@` or `}` | `FMT_BEGIN_NAMESPACE`, `G_BEGIN_DECLS`, `NS_ASSUME_NONNULL_BEGIN` |

A macro is an identifier of capitals, digits and underscores, at least two
characters, optionally followed by a parenthesised argument list. The last two
rows and the head forms were added because the projects below wrote them;
that is the whole reason they are there. Not recognised: mixed-case macros
(`Py_LOCAL_INLINE(int)`), a macro that is a statement (`FMT_CATCH(..) { }`,
`TEST_F(a, b) { }`), and anything inside a function body.

**The guard.** An upper-case identifier is often a type (`FILE *open(void)`,
`BOOL WINAPI Foo(void)`, `DFA* Prog::Get(`, `DWORD x;`), an enum value or a macro
call statement, so a mask is only ever tried and never trusted:

1. The plain source is parsed first, and a file with no candidate costs nothing
   more and is returned as it is.
2. The masked source is parsed only if the plain parse shows trouble where a
   candidate sits: an error or missing node overlapping one, a definition named
   after one, or (new, and needed) no definition at the name the text says the
   declaration defines. The last matters because the grammar parses `class
   EXPORT Foo { };` as a function with no error node at all.
3. The masked result replaces the plain one only if it has no more error nodes
   (a missing token counts as one) and no fewer definitions, with at least one
   of the two strictly better; no definition named after a masked token; and
   every definition the plain parse had, except one named after a masked token,
   still present. Otherwise the plain result stands, byte for byte.

A file is parsed at most twice by this mechanism, the second parse goes through
`parseOnce` and so has the same work limits, wall-clock ceiling and
cancellation as any parse, and a stopped parse is never retried. The count is on
`Result.MacrosMasked`, and a partial parse's stored error text ends "read with N
macro tokens masked".

The guard is all-or-nothing for a file, and that costs something: one wrong
candidate vetoes the good ones beside it. Measured case: `redis/src/cluster_legacy.c`
has one candidate, `REDIS_NO_SANITIZE("bounds")`; masking it moves the file's one
syntax error from line 4089 to line 5222 and loses one definition, so the plain
parse is kept and the file still has the error.

### Two definition-query fixes in the same change

`lang_c.go` and `lang_cpp.go` replace the query the library infers.

- A forward declaration (`class Cache;`, `struct Foo;`, `typedef struct Foo
  Foo_t;`) is not a definition. A class or struct is one only with a body. The
  inferred query matched any `struct Foo` written anywhere, parameter types
  included, which is why redis loses half of its "class" symbols (3,238 to
  1,578); those were uses, not definitions.
- A function-pointer typedef declares its own name and not its return type's:
  `typedef Iterator* (*BlockFunction)(void*, const ReadOptions&);` was a
  declaration of `Iterator` and now declares `BlockFunction`.
- Checked first and found broken: a function returning a pointer (`char
  *name(int a) { }`) and an out-of-line method (`void Table::Open(..) { }`)
  matched nothing. Redis goes from 11,160 to 12,855 functions mostly from the
  first; leveldb gains 257 methods and re2 389 from the second, each owned by its class
  (`path:Table.Open`, and `ns::Table::Open` too). Out-of-line destructors,
  operators and `using` aliases are still missed (`fixtures/langs/cpp/KNOWN_GAPS.md`).

### Measured

Tarballs of the default branches (not clones, so no commit is named) on 13 November 2026 (leveldb `main`, redis
`unstable`, re2 `main`, fmt `main`); a binary built at `c7a6b45` and one built
from this change; `init`, `index`, `resolve`. Files here are the C, C++ and
Objective-C ones the index parsed. "Macro-named" counts class, function and
method symbols whose name is capitals with an underscore.

| Project | clean, before to after | with syntax errors | incomplete | symbols | files masked (tokens) | macro-named symbols |
|---|---|---|---|---|---|---|
| leveldb (C++) | 96 to 108 | 37 to 25 | 0 to 0 | 1,405 to 1,697 | 21 (127) | 210 to 178; `LEVELDB_EXPORT` itself 23 to 0 |
| redis (C) | 448 to 464 | 408 to 392 | 0 to 0 | 14,398 to 14,434 | 43 (423) | 265 to 248 |
| re2 (C++) | 54 to 54 | 23 to 23 | 1 to 1 | 954 to 1,360 | 3 (3) | 1 to 1 |
| fmt (C++) | 32 to 34 | 40 to 38 | 2 to 2 | 1,544 to 1,672 | 17 (665) | 28 to 15 |
| JSONKit.m (Objective-C) | 0 to 0 | 1 to 1 | 0 to 0 | 195 to 206 | 1 (20) | 0 to 0 |

The macro-named symbols that remain are statements, not declarations:
`TEST_F` (176 in leveldb), `TEST_BEGIN` (214 in redis), `FMT_CATCH`,
`LOCKS_EXCLUDED` in the unmasked files. The most-masked tokens are fmt's
`FMT_CONSTEXPR` (217 uses), then `GTEST_API_` (80), `FMT_API` (38) and
`FMT_BEGIN_NAMESPACE` (32); redis's `JEMALLOC_ALWAYS_INLINE` (86),
`JEMALLOC_EXPORT` (31), `REDISMODULE_API` (29), `CROARING_TARGET_AVX2` (13);
leveldb's `LEVELDB_EXPORT` and `LOCKS_EXCLUDED(..)`.

JSONKit.m: 45 functions to 56, 20 macro tokens masked. The 11 recovered are the
`JK_STATIC_INLINE` functions. The file still reports a syntax error (unchanged).

**Cost.** `index` wall time and user CPU, means of three runs, binaries
alternated. A masked parse is a second parse of the whole file, and
gotreesitter reads about 80 KB of C++ a second per core, so the cost is the size
of the files that are retried.

| Project | wall, before to after | user CPU | peak memory |
|---|---|---|---|
| leveldb | 1.33 s to 1.41 s | 2.36 s to 2.58 s | 290 MB to 336 MB |
| re2 | 1.59 s to 1.99 s | 2.68 s to 3.67 s | 341 MB to 536 MB |
| fmt | 2.53 s to 3.35 s | 4.99 s to 6.66 s | 673 MB to 929 MB |
| JSONKit.m | 2.24 s to 4.42 s | 2.24 s to 4.38 s | 273 MB to 424 MB |
| redis | 15.8 s to 18.3 s (runs 1 and 2) | 29.5 s to 34.2 s | 790 MB to 750 MB |

Redis run 3 (29.95 s and 31.26 s) ran while other work was using the machine and
is left out of the means. re2 retried 157 KB in three files, which is the whole
of its 1 s; JSONKit.m is one 177 KB file parsed twice.

### Definitions, by reading

Five files per project chosen by a fixed rule: the five largest non-test
sources or headers by bytes. The rule picks leveldb's five `.cc` files, which
hold few export macros, so its five largest public headers (where they are) are
read as a second set. "Expected" is every definition a developer would list:
functions, methods, classes and structs with a body, typedef names. It started
from a text enumerator that tracks braces and namespace and class nesting and
does not use tree-sitter, and every name on which it and the index disagreed
was then read in the source: 21 names the enumerator missed were real
(function-local structs and their members such as `LogReporter::Corruption`,
`static struct config { .. }` and `typedef enum X { .. }` in redis,
function templates after a template header in fmt) and were added to
"expected" and counted correct; the rest are as the enumerator said. Counts are
distinct names. "Spurious" is an extracted name nothing in the file defines: a
macro, a forward declaration, a type that is only used.

| File | Expected | Correct, before to after | Spurious | Missing |
|---|---|---|---|---|
| leveldb `db/version_set.cc` | 79 | 37 to 76 | 0 to 0 | 42 to 3 |
| leveldb `db/db_impl.cc` | 49 | 11 to 49 | 0 to 0 | 38 to 0 |
| leveldb `util/env_posix.cc` | 60 | 4 to 4 | 0 to 0 | 56 to 56 |
| leveldb `util/env_windows.cc` | 54 | 9 to 9 | 0 to 0 | 45 to 45 |
| leveldb `util/crc32c.cc` | 4 | 3 to 4 | 0 to 0 | 1 to 0 |
| **leveldb, five largest** | 246 | 64 to 142 | 0 to 0 | 182 to 104 |
| leveldb `include/leveldb/env.h` | 27 | 6 to 6 | 2 to 0 | 21 to 21 |
| leveldb `include/leveldb/c.h` | 16 | 16 to 16 | 0 to 0 | 0 to 0 |
| leveldb `include/leveldb/options.h` | 4 | 1 to 4 | 7 to 0 | 3 to 0 |
| leveldb `include/leveldb/db.h` | 3 | 2 to 3 | 5 to 0 | 1 to 0 |
| leveldb `include/leveldb/cache.h` | 4 | 4 to 4 | 1 to 0 | 0 to 0 |
| **leveldb, five largest headers** | 54 | 29 to 33 | 15 to 0 | 25 to 21 |
| redis `src/module.c` | 661 | 516 to 661 | 8 to 0 | 145 to 0 |
| redis `src/redis-cli.c` | 260 | 233 to 259 | 7 to 0 | 27 to 1 |
| redis `src/server.c` | 257 | 237 to 256 | 18 to 0 | 20 to 1 |
| redis `src/t_stream.c` | 147 | 134 to 147 | 0 to 0 | 13 to 0 |
| redis `src/cluster_legacy.c` | 211 | 173 to 208 | 6 to 0 | 38 to 3 |
| **redis, five largest** | 1,536 | 1,293 to 1,531 | 39 to 0 | 243 to 5 |
| re2 `re2/unicode_groups.cc` | 0 | 0 to 0 | 0 to 0 | 0 to 0 |
| re2 `re2/parse.cc` | 68 | 24 to 68 | 0 to 0 | 44 to 0 |
| re2 `re2/dfa.cc` | 58 | 22 to 56 | 0 to 0 | 36 to 2 |
| re2 `re2/re2.h` | 76 | 55 to 64 | 2 to 0 | 21 to 12 |
| re2 `re2/re2.cc` | 42 | 9 to 40 | 0 to 0 | 33 to 2 |
| **re2, five largest** | 244 | 110 to 228 | 2 to 0 | 134 to 16 |
| fmt `include/fmt/format.h` | 214 | 13 to 13 | 0 to 0 | 201 to 201 |
| fmt `include/fmt/core.h` | 88 | 0 to 0 | 0 to 0 | 88 to 88 |
| fmt `include/fmt/format-inl.h` | 80 | 58 to 61 | 2 to 1 | 22 to 19 |
| fmt `include/fmt/chrono.h` | 141 | 0 to 0 | 0 to 0 | 141 to 141 |
| fmt `include/fmt/ranges.h` | 42 | 0 to 0 | 0 to 0 | 42 to 42 |
| **fmt, five largest** | 565 | 71 to 74 | 2 to 1 | 494 to 491 |

The remaining spurious name is `FMT_CATCH` in `format-inl.h`, a macro statement.
Most of what is still missing is one cause, the `#if` problem below: both leveldb
env files, `format.h`, `core.h`, `chrono.h` and `ranges.h` have a single error
node over the whole file and extract close to nothing. The rest is out-of-line
destructors and operators, which the query does not match.

### Hierarchy on leveldb

`EXTENDS` edges, all of them read against the declaration:

| | Edges | `high` | `medium` | `low` | Right |
|---|---|---|---|---|---|
| before | 24 | 6 | 15 | 3 | 10 of 24 |
| after, `medium` as it was | 44 | 5 | 15 | 24 | 34 of 44 (`high` 5 of 5, `medium` 5 of 15, `low` 24 of 24) |
| after, C++ `medium` made `low` | 44 | 5 | 0 | 39 | 34 of 44 (`low` 29 of 39) |

All of the 24 `low` edges are subclasses of the public classes, now bound to the
class in `include/leveldb` (`WritableFile`, `FilterPolicy`, `Logger`, `DB`,
`Snapshot`, ...). The 10 wrong `medium` ones are the ones the earlier section
describes, and the cause is the same: a subclass beside a local `struct
Comparator` in `skiplist_test.cc`, a local `Handler` in `db_test.cc` (for
`WriteBatch::Handler`, whose qualifier is not carried) or `SkipList::Iterator` in
`skiplist.h` (for `leveldb::Iterator`) is the only candidate in the same
directory, and `pickSameDir` takes it at `medium`. With the real classes now in
the index there are two candidates and the nearer one is still chosen.

**Tier changed.** `medium` for a C++ base class found by the same-directory rule
was 5 of 15, below the 60% floor, so in `pick()` in `resolver.go` that one case
is `low` (the reason ends "C++ base class by name only"). It is C++ only, and
only for `extends`; calls do not pass through it. The 15 edges become `low`; 5
of them are right, 10 wrong, which is `low`'s lot.

Recall on the five files of the earlier measurement (`db/db_impl.cc`,
`db/version_set.cc`, `benchmarks/db_bench.cc`, `util/env_posix.cc`,
`util/env_windows.cc`; 16 relationships): 2 of 16 before and 2 of 16 after. The
change did not help here, for three reasons that are not macros:
`env_posix.cc` and `env_windows.cc` (12 of the 16) are wrapped in one error node
because of `#if` inside a constructor's initializer list, so their classes are
not symbols; `class Version::LevelFileNumIterator : public Iterator` has a
qualified name and no class symbol; and `CountComparator : public Comparator`
in `db_bench.cc` is ambiguous (two files define `Comparator`, none is beside
it), so it binds to nothing.

### Calls on leveldb

`CALLS` edges, `high` / `medium` / `low`: 1,442 / 78 / 508 (2,028) before and
2,029 / 540 / 973 (3,542) after. 1,629 edges are new and 115 are gone (114 `low`,
1 `high`); the gone ones are `env->RemoveFile` and the like bound to
`VersionEdit::RemoveFile` as "the only member of this name", which is now an
ambiguous name and, where the receiver is typed, a right one. No edge moved
tier.

Thirty of the 1,629 new edges, drawn at random (seed 42 over the new edges
ordered by source, name, destination and line) and judged by reading the call
and the target:

| Tier | Sampled | Right | Wrong |
|---|---|---|---|
| `high` | 11 | 9 | 2 |
| `medium` | 8 | 8 | 0 |
| `low` | 11 | 8 | 3 |

The 5 wrong: `iter_->key()` at `db_iter.cc` lines 123 and 216, bound at `high`
to `Block::Iter::key` because the receiver `iter_` is "named after the type"
`Iter`, when `iter_` is an `Iterator*` holding whatever the database was given;
and `Lookup(..)` twice and `Insert(..)` once in `cache_test.cc`, calls to the
test fixture's own methods, bound at `low` to `LRUCache::Lookup` and
`::Insert` because the fixture class is not the enclosing symbol of a `TEST_F`.
Two of 11 `high` edges wrong is a sample too small to move a tier, and the rule
that made them (`receiver is named after the type that declares it`) is in the
resolver, not in this change; it is reported here and not touched.

### What the mask did not help with

- **`#if` / `#else` through a declaration or an initializer list.**
  `leveldb/util/env_posix.cc` and `env_windows.cc` (`#if !defined(NDEBUG)` inside
  a constructor's initializer list), `env.h` and `fmt/core.h` (`#if` and `#undef`
  around class members): the grammar wraps the whole file, or the rest of it,
  in an error node, and every definition in it is lost. A reduction of the
  first:

  ```text
  class Limiter { public: Limiter(int n) : a_(n),
  #if !defined(NDEBUG)
        max_(n),
  #endif
        x_(0) {} };
  int after() { return 1; }

  (translation_unit
    (ERROR (ERROR (_class_name (type_identifier)) (_field_declaration_list_item (access_specifier))
        (function_declarator (identifier) (parameter_list ..)) (field_initializer ..))
      (unary_expression (preproc_defined (identifier))) .. (argument_list (number_literal)))
    (compound_statement (ERROR)) (expression_statement)
    (function_definition (primitive_type) (function_declarator (identifier) ..)))
  ```

  Only `after` is a symbol. Masking cannot remove a directive without choosing
  a branch, so this is not attempted.
- **A class with a qualified name.** `class Version::LevelFileNumIterator :
  public Iterator { .. };` is
  `(class_specifier (qualified_identifier (namespace_identifier) (type_identifier))
  (base_class_clause ..) (field_declaration_list ..))`, a name that is not a
  `type_identifier`, so there is no class symbol and no hierarchy edge.
- **A function-like macro that opens a body.** `TEST_F(CacheTest, HitAndMiss) {
  .. }` is `(function_definition (function_declarator (identifier)
  (parameter_list (parameter_declaration (type_identifier)) ..)) (compound_statement ..))`:
  a function named `TEST_F`, which is what the text says.
- **The mask rejected by the guard.** `redis/src/cluster_legacy.c`, above.
  `src/bio.c:189`, `src/acl.c:77` and `src/cluster.c:174` report errors in
  function bodies and a table initialiser at no macro; they were not diagnosed.
- **A clean parse that is wrong.** The grammar reads `class EXPORT Foo { };` as
  a function with no error node; the mask handles that case by asking whether
  the name the text says is defined is a definition. A clean wrong parse of some
  other shape would not be noticed, and finding one needs a compiler.

### Tests

`internal/parse/cmacro_test.go`: each candidate form and each non-candidate (a
library type, a macro call statement, an enum value, a comment, a string, a
`#define` body, a macro loop after a condition); the same-length and
newlines-kept property; the guard accepting a mask that helps and refusing one
that does not (`FILE *`, `BOOL WINAPI`, an extra syntax error); a clean file
untouched and not retried; a clean parse with a candidate not retried;
cancellation; out-of-line method ownership; forward declarations and
function-pointer typedefs. `fixtures/langs/c` and `fixtures/langs/cpp` are the
conformance fixtures for the two queries, and `fixtures/receivers/cpp/exported.cc`
pins a subclass of an export-macro class whose inherited `this->Fetch()` binds to
the base's method and not to an unrelated class's.

## 2026-11-12: calls in PHP, Kotlin and Scala, and `new` in Java

PHP, Kotlin and Scala recorded definitions and not one call, so every "who
calls this" answer was empty while the tier promised cross-file references.
Java recorded no `new Widget(3)`, so a class that was only constructed looked
unused. All four now record them. Projects: guzzle/guzzle (PHP), square/okio
(Kotlin), scopt/scopt (Scala), google/gson (Java), shallow clones, `init`,
`index`, `resolve`. Every edge below was judged by reading the caller and the
definition, strictly (bound to a same-named method of another class is wrong;
unclear counts as not right), by sub-agents of an AI assistant. Samples are
random with a fixed order (a multiplicative hash of the edge id), 30 per tier
(all, when fewer), across rules.

### What is extracted

| | Now extracted | Not extracted |
|---|---|---|
| PHP | `f()`, `$x->m()`, `$x?->m()`, `A::m()`, `self/static/parent::m()`, `new A`; `use` (aliases, `use function`, groups) as dotted paths; `extends`, `implements`, trait `use` | `$f()`, `new $cls`, `new static`; the receiver of a name written with `\` (`\A\B::m()` keeps the call, loses the receiver); `$x?->m()` records the receiver as `x?`; `(new A)->m()` has an unreadable receiver |
| Kotlin | `f()`, `x.m()`, `x?.m()`, `A.m()`, `this/super.m()`, `A(...)` constructor calls, extension calls; `import` with aliases; supertypes; classes with no body; `object` (now a variable, not a function) | infix and operator calls (`a and b`), declared types of parameters (the grammar has no fields) |
| Scala | `f()`, `x.m()`, `x m y` (infix by name), `f[T](x)`, `A(...)`, `new A`, `new p.A`, `new A {}`; imports with selectors, renames and wildcards; `extends ... with ...`; `object` members | calls written without parentheses (`x.size`, `x.desc`) |
| Java | `new A()`, `new p.A()`, `new A<T>()`, `outer.new A()`, `new A() {}` as INSTANTIATES to the class | `this(...)` and `super(...)` delegation |

An anonymous class constructs the type it names. It binds when that type is a
class; `new Runnable() {}` names an interface, which is never instantiated, so
it binds to nothing.

### Before and after (`repocortex resolve`)

| Project | References | Resolved | Ambiguous | Unresolved (external) | Edges | `INSTANTIATES` edges |
|---|---:|---:|---:|---:|---:|---:|
| gson, before | 20,468 | 8,332 | 1,230 | 10,906 (6,620) | 8,263 | 0 |
| gson, after | 24,068 | 10,930 | 1,274 | 11,864 (7,290) | 10,794 | 2,531 |
| guzzle, before | 0 | 0 | 0 | 0 | 0 | 0 |
| guzzle, after | 23,254 | 7,787 | 1,801 | 13,666 (1,826) | 7,753 | 2,242 refs resolved |
| okio, before | 504 | 67 | 26 | 411 (112) | 67 | 0 |
| okio, after | 23,806 | 5,022 | 9,657 | 9,127 (1,582) | 4,790 | 464 refs resolved |
| scopt, before | 0 | 0 | 0 | 0 | 0 | 0 |
| scopt, after | 2,653 | 684 | 656 | 1,313 (75) | 681 | 52 refs resolved |

gson's `CALLS` edges are the same set before and after: the same 8,053, equal
by file, line, target and confidence. Its `high` tier grew by the 2,460
`INSTANTIATES` edges. okio's 9,657 ambiguous references are mostly names that
a multiplatform project declares once per source set (`Buffer`, `toPath`,
`writeUtf8`).

Edges per tier after the tiers below were set:

| Project | high | medium | low |
|---|---:|---:|---:|
| gson | 8,245 | 143 | 2,406 |
| guzzle | 5,824 | 30 | 1,899 |
| okio | 2,281 | 763 | 1,746 |
| scopt | 558 | 10 | 113 |

### Precision (right / judged)

Sampled with the tiers that were in force when the samples were drawn.

| Project | high | medium | low |
|---|---:|---:|---:|
| gson (`CALLS` and `INSTANTIATES`) | 30 / 30; `INSTANTIATES` alone 30 / 30 | 25 / 30 | not drawn (unchanged) |
| guzzle | 30 / 30 | 30 / 30 | 11 / 30 |
| okio | 29 / 30 | 21 / 30 | 19 / 30 |
| scopt | 30 / 30 | 25 / 25 | 20 / 30 |

Java `high` did not fall on gson: the `CALLS` set is identical and the new
`INSTANTIATES` edges were 30 / 30 (and 20 / 20 for the rule alone).

By rule, where a rule decided a tier:

| Rule | guzzle (PHP) | okio (Kotlin) | scopt (Scala) | Tier set |
|---|---:|---:|---:|---|
| unknown receiver, the only member of that name | 7 / 20 (the low tier, all this rule: 11 / 30) | 7 / 20 (the low tier: 19 / 30) | 20 / 20 | PHP low, Kotlin low, Scala medium |
| receiver named after the type | 20 / 20 | 15 / 20 (3 wrong, 2 unclear) | 20 / 20 | PHP high, Kotlin medium, Scala high |
| type identified only as the one of that name | 20 / 20 | 3 / 3 | 1 / 1 | PHP high |
| `new T` / `T(..)` bound to the class | 20 / 20 (+30 / 30 in medium) | 18 / 20 | 15 / 15 | follows the previous row |
| only function of the name, called as a method (extension) | none | 18 / 20 | none | Kotlin high |
| overloaded, the first declaration | | 1 / 5 | 5 / 14 | unchanged: low |

The wrong edges have three causes. A call on a library object bound to the
repository's only method of that name: PSR interface methods and PHPUnit's
`self::fail` in guzzle (13 of 20), the JDK's `Class.isAssignableFrom` in gson
(5 of 9), the standard library's `Int.toLong` bound to a private extension in
okio (12 of 13). A type name that exists twice (`File`: `java.io.File` and the
fake file system's, 2 of 20 constructions in okio). And the first declaration
of an overloaded name, which the index cannot choose among because Kotlin and
Scala record no parameter counts.

### Recall

The five largest non-test source files of each project, the first 8 call
sites per file that target a definition in the repository, decided by reading
before the index was consulted. okio's `BufferedSource.kt` has no qualifying
site (an `expect` interface), hence 26.

| Project | Sites | Bound right | Bound wrong | Unbound |
|---|---:|---:|---:|---:|
| gson | 40 | 38 | 1 | 1 (`this(...)`) |
| guzzle | 40 | 40 | 0 | 0 |
| okio | 26 | 5 | 2 | 19 (2 not extracted) |
| scopt | 40 | 17 | 1 | 22 (18 not extracted) |

guzzle's sample is mostly `new` of exception classes and `self::` calls, so
its 100% is the easy end. okio's unbound sites are calls to names declared once
per source set (ambiguous) and bare calls with an implicit extension receiver.
Of scopt's 18 not extracted, 11 are accessors called without parentheses
(`option.desc`), which are not call expressions in the grammar. The wrong
bindings are overload choices (`append(cs, 0, n)` bound to the one-argument
`append`) and a `this(...)` bound to the constructor it is written in.

### Tiers

`CallTraits.Measured` was set from the tables above: PHP `NamedAfterType` and
`UniqueTypeName` high (20 / 20 each), Kotlin `Extension` high (18 / 20),
Scala `NamedAfterType` high (20 / 20) and `UnknownReceiver` medium (20 / 20,
but every one of them was `programName` under one `import builder._`, which is
too narrow to earn `high`). PHP and Kotlin unknown receivers measured below the
`medium` floor of 60% and stay `low`. `parse.TierFor` still derives all three
languages' tier from their queries: `resolved`, because none has an arity
query or a scope analysis, so none can reach `typed`.

### Method notes

PHP's namespaces are mapped to directories by Composer, not by the file, so a
`use` path is matched to a class by that class being the one of its name, not
by its directory. Kotlin's grammar defines no fields; the scope walk reads a
class's or function's name and body by position instead. A Scala `object`
opens a scope, and a statement written directly in a Kotlin or Scala type's
body is a member's caller.

## 2026-11-13: Rust impls, merged clauses, the SQL scanner, and library calls in PHP and Kotlin

Four defects, each measured on a shallow clone with `init`, `index`, `resolve`.
Where this section and an earlier one disagree, this one is later: the note
above that Rust `impl Trait for Type` is not recorded is superseded by item 1.

### 1. Rust `impl Trait for Type` is now `IMPLEMENTS`

The reference sits on the impl block, which is not a symbol, so the edge had no
source. The tags query now captures the type named by `type:` as well, the
parser carries it on the reference (`Receiver`, with the new
`model.ReceiverImplementer` shape; no call has that shape), and `bind()` starts
the edge at that type: the symbol of the name in the impl's own file, else the
one type of that name in the repository, else the one in the same directory,
else no edge (the trait reference is still recorded as resolved). The trait and
the type each take the four spellings `T`, `T<A>`, `path::T`, `path::T<A>`,
read by their last segment.

Recorded nothing, deliberately: `impl Type { }` (inherent), `impl<T: Bound>
Trait for T` (blanket: the type is one of the impl's own type parameters, so
there is no single type; dropped in the parser because a query cannot compare
two nodes), `impl Trait for &T`, `[T]` and `(A, B)` (no type name), and the
standard-library traits on the existing list, for the reason given there.

ripgrep (HEAD, 247 `impl .. for ..` lines):

| | before | after |
|---|---:|---:|
| `IMPLEMENTS` edges | 0 | 125 (high 7, medium 105, low 13) |
| `CALLS` high / medium / low | 3134 / 870 / 419 | 3136 / 870 / 419 |
| `INSTANTIATES` high / medium / low | 172 / 2 / 1 | 172 / 2 / 1 |

151 impl references were captured (the rest are standard-library traits, blanket
impls and impls for references); 128 resolved, to the trait in the repository,
and 125 of those had a source. The 23 unresolved name traits from other crates.

Precision: 30 of the 125 drawn with `random.seed(42)`, judged by reading the
impl line and the trait definition: 30 right (3 of them `low`, all right). 100%
with a wide interval on 30; 95 of the 125 are one trait, `Flag`.

Recall: the five largest non-test files (`defs.rs`, `standard.rs`, `walk.rs`,
`glob.rs`, `dir.rs`), every `impl Trait for Type` whose trait is defined in the
repository: 106 of 107 have an edge. The miss is `impl<'a, P: ..> Trait for &'a
mut P`, which names no type.

New call edges: 2, not 20, both `high` and both right: `self.shortest_match()`
inside `impl Matcher for RegexMatcher` now reaches `Matcher::shortest_match`,
the trait's default method, through the hierarchy. No tier moved and the Rust
row of `calltraits.go` is unchanged.

`repocortex implementations` before and after:

```
Sink      before: Implemented or extended by (0)
          after:  (7) UTF8, Lossy, Bytes (high); KitchenSink (medium);
                      JSONSink, StandardSink, SummarySink (low)
Matcher   before: Implemented or extended by (0)
          after:  (5) RegexMatcher x3 and RegexMatcherNoCaps (low, across crates)
```

### 2. A function written as clauses is one symbol

Elixir's query matches one clause head at a time, and the Erlang grammar makes
each clause its own `fun_decl`, so a function of five clauses was five symbols
of one name. `registerClauseMerge(language, arity)` (in `clausemerge.go`) folds
consecutive definitions of a file that agree on name, kind, owner and head arity
into one symbol from the first clause to the last. Elixir and Erlang are the
only languages of the tree that write functions this way (Julia's methods are
different methods, and Haskell has no query). Arity is available: it is the
number of arguments written in the head, so `def f(a)` and `def f(a, b)` stay
two symbols, `def f(a, b \\ 1)` is f/2, and a clause with another symbol of the
same module between it and the earlier ones is not merged. Only functions and
methods merge; a repeated module attribute is not a clause.

| File | symbols before | after |
|---|---:|---:|
| fixtures/langs/elixir/shop.ex | 39 | 34 |
| fixtures/langs/elixir/worker.ex | 24 | 21 |
| linguist Elixir `float.ex` | 61 | 31 |
| linguist Elixir `integer.ex` | 34 | 22 |
| linguist Elixir `regex.ex` | 83 | 37 |
| linguist Elixir `atom.ex`, `function.ex` | 4, 6 | 4, 6 |

(The fixture counts are before the three `pack` lines were added to `shop.ex`
to pin the non-merging cases.) Every same-named pair left in those files was
read: all are different arities (`compile/2`, `/3` and `/4`, `round/2` and
`/3`, `decompose/2` and `/4`, `to_string/1` and `/2`).

### 3. The SQL table scanner

| | cause | change |
|---|---|---|
| quoted name with a space produced no table | the tokenizer stripped the quotes and the bare word `Line Items` then failed the identifier check; a quoted keyword (`"order"`) failed the keyword check | a quoted token keeps its quotes, a qualifier is joined to it (`"shop"."Line Items"`, `[dbo].[Orders]`), and a quoted name is a name. Backticks, brackets and double quotes with spaces all work. Column lists read quoted names the same way, which also recovers every column of a backtick-quoted MySQL table (they were all rejected, the first character being a backtick) |
| `accounts` at line 1, declared at line 3 | the file splitter started a statement at the first non-blank character after the previous `;`, and a comment is one | a statement starts at its first character that is not white space or part of a comment |
| `DROP TABLE scratch` declared `scratch` | any DDL statement (CREATE, ALTER, DROP, CREATE INDEX) created the table symbol | ALTER, DROP and CREATE INDEX are marked `Existing` and stay accesses; only CREATE TABLE (and what an ORM entity declares) creates the symbol. A table only ever used keeps its accesses and is reported undeclared, as before: no symbol is created for it |

Indexing a copy of `fixtures/langs/sql/`, `table` symbols:

```
before                        after
mysql.sql   1 accounts        mysql.sql   3 accounts
mysql.sql  10 transfers       mysql.sql  10 transfers
mysql.sql  23 audit_log       mysql.sql  23 audit_log
mysql.sql  42 scratch         (gone; the DROP is still a DDL access)
schema.sql  5 customers       schema.sql  5 customers
schema.sql 11 orders          schema.sql 11 orders
                              schema.sql 17 line items
schema.sql 22 scratch         schema.sql 22 scratch
schema.sql 24 copy_of_orders  schema.sql 24 copy_of_orders
```

`column` symbols: `schema.sql` gains the two of `line items`; `mysql.sql` goes
from none to nine. No pinned test expectation moved.

### 4. Library calls bound to a lone repository namesake (PHP, Kotlin)

`memberByName` binds `x.f()` on an `x` of unknown type when exactly one type in
the repository declares an `f`, and `corenames.go` lists the names a language's
own library declares, for which that is not evidence. PHP had seven names and
Kotlin shared Java's list. Measured with 30 `low` `CALLS` edges drawn with
`random.seed(42)`, judged by reading, before and after:

| | low edges | sample right, before | sample right, after | removed edges right |
|---|---:|---:|---:|---:|
| guzzle | 1899 to 734 | 3 / 30 | 27 / 30 | 0 of 30 sampled (of 1165) |
| okio | 1500 to 892 | 19 / 30 | 27 / 30 | 1 of 608 |
| scopt (Scala, unchanged) | 103 | 30 / 30 | 30 / 30 | none removed |

`high` and `medium` did not move in any of the three (guzzle 3563 / 0, okio
1948 / 757, scopt 501 / 4). guzzle's removed edges were PSR-7 methods
(`getBody` 229, `getHeaderLine` 227, `hasHeader` 109, `getUri` 97, `getContents`
51 ...) and PHPUnit's `fail` (320), bound to a test double or a private helper;
okio's were all `toLong`, bound to the one private `LARGE_INTEGER.toLong()`,
which is visible in one file, and the one right edge is in that file.

PHP gains the PSR-7/17/18 interface methods, PHPUnit's fixed names and its
`assert*`, `expect*` and `markTest*` families (by prefix, with a capital after
it), and the exception, closure and `Iterator`-style names. Kotlin gets its own
list over the JVM's: number conversions, scope functions, and the string and
collection extensions. Left out on purpose, after the first version of the list
removed correct edges: Kotlin's `use` (okio declares its own for platforms
without `kotlin.io`; 211 `high` edges to it were right), operator names
(`div`, `plus`), `copy`, and PHP's reflection and `IteratorAggregate` names
(`getName`, `setValue`, `getIterator`), which an application's own cookie or
collection class declares, and which a call on a typed variable binds anyway.
Scala shows no such pattern on scopt and has no entry.

Still wrong: `Psr7\DiagnosticValue::escape()` (a static call on an imported
library class) binds to an unrelated private `escape`; `$promise->cancel()` and
ReflectionProperty's `setValue` bind to a lone repository method of the name;
Kotlin's `inflated.utf8()` binds to a test function named `utf8` although
`ByteString` declares `utf8` (in an `expect` class and its `actual` ones; why
those do not count as members was not diagnosed); `randomBytes(..)`
from a `nativeTest` source set binds to a `jvmTest` helper of the same name.

## 2026-11-13: the second verification, and what is still wrong

The [verification record of 13 November](verification/2026-11-13/README.md) repeats the hand check of the record of 12 November on the same 14 projects, at the same commits and the same files, after the fixes (build `8748b29`), draws fresh samples, and adds three things: definitions in 36 further languages, two very large repositories, and a comparison of every edge and symbol on the 14 projects between that build and the final one (`61f5992`). Every figure below is in that record with its denominator. It is the same strict reading as the first: a call bound to a same-named function of another class or package is wrong, and an edge that could not be settled is not right.

| | Round 1 (build `53246ac`) | Round 2 (build `8748b29`) |
|---|---:|---:|
| `high`, right / judged, 14 projects | 396 / 420 (94%) | 405 / 420 (96%) |
| `medium` | 207 / 272 (76%) | 249 / 259 (96%) |
| `low` | 181 / 276 (66%) | 184 / 280 (66%) |
| Call-site recall, first eight sites per file | 393 / 529 (74%) | 450 / 529 (85%) |
| Definitions in the five files, correct / expected | 2,996 / 3,113 | 3,081 / 3,106 |
| Spurious definitions | 153 | 12 |

Round 2 is not a fresh sample: it is the same projects, and several of the fixes were made after reading them. It shows that the fixes held there. The samples differ between rounds (round 2 drew new edges), so a row is not a paired comparison.

### What this supersedes above

- **The "Result" section's reading** ("`high` is worth acting on, in every language measured. `medium` is not.") is superseded for the languages it did not cover. The compiler-index figures in that section stand for the repositories they were measured on. On 14 other projects, read by hand, `high` was below the 86.8% floor on google/benchmark (C++, 22 of 30 in both rounds; in round 2 none was wrong and 8 were unclear) and moshi (Kotlin, 26 of 30 in round 2, from 29 of 30), and `medium` was right on 249 of 259 sampled edges in round 2 wherever the tier had edges. The "Receivers" floors are therefore a description of the nine projects of 12 November and not a guarantee for every project.
- **"Receivers", "What is still wrong"**: the last bullet says PHP, Kotlin and Scala extract no references and were not measured. They record calls since the section of 12 November below, and round 2 measured recall of 33 of 40 sites in PHP, 40 of 40 in Kotlin (27 in round 1) and 30 of 40 in Scala (20 in round 1).
- **"Inheritance for Ruby, C#, Rust and C++"**: its note that Rust `impl Trait for Type` is not recorded is superseded by the Rust item of the section of 13 November below; its C++ hierarchy figures are superseded by round 2, which sampled 20 of 33 C++ edges, all pointing at the declared base.

### Known gaps

Each is stated with the figure that measured it, from the record or from the commit named.

- **The `low` tier is poor.** Right / judged in round 2: Python 8 / 30, Kotlin 9 / 30, C# 14 / 30, Java 18 / 30. Use `--min-confidence medium` for anything that matters.
- **Overloads of the same arity.** The binder uses the argument count and not the argument types. Wrong `low` edges in round 2: Java 12 of 30, Kotlin 21 of 30, C# 15 of 30. In C++ google/benchmark's `FormatKV` (47 references) all bind to the first overload, now at `low`.
- **Go has no interface-implementation edges.** Zero `EXTENDS` or `IMPLEMENTS` edges for Go in both rounds (621 Go definitions in round 1); `find_implementations` answers `UNKNOWN`.
- **A call at module level had no caller symbol** and so made no edge (express 2,015 of 2,180 resolved references, zod 2,952 of 7,540, taxonomy 20 of 429, JavaScript section above). `resolve` now attributes code outside every definition to a symbol of kind `file`, `(top level of <path>)`, per file. What this does not change: a call the parser does not record still has neither a reference nor an edge (on honojs/hono the one site `find_callers` misses against the TypeScript compiler is `c.req.json<{ name: string }>()` inside `c.text((await ...)['name'])`, `src/middleware/csrf/index.test.ts:7`, where the parser records `text` and not `json`).
- **Calls on a value need type inference.** On VS Code (build `a75b887`) 426,853 references were left unbound because the name has more candidates than the cap; after the second look at those names (commits `3ca1c04`, `7736265`) the final build leaves 421,929 unbound there, 714,763 on Elasticsearch and 60,054 on the Linux kernel, and binds 59.3%, 66.5% and 43.8% of the references that name something indexed. Calls in a chain that starts from `new` or a call are not extracted in C# (4 of 8 recall sites in one file).
- **Definitions in languages whose grammar cannot read common constructs.** Correct / expected, build `f783fa5`, before the later fixes and not re-read since: F# 23 / 186, Make 22 / 142, Groovy 72 / 127, Objective-C 80 / 118, OCaml 311 / 408, Swift 179 / 219, Julia 174 / 185. Commits `3152579`, `1235cd5`, `71df469`, `3f24c89`, `9f52948`, `24233f3`, `8cc57c2` address them; Alamofire's `DownloadRequest.swift` went from 13 to 35 of 41 definitions in the last. With the source mask for the two constructs that cost whole files (commits `a5be0b0`, `5298b1f`), the files of the check extract 186 of 186 (F#) and 127 of 127 (Groovy) by count; on two further F# projects the five largest files gave 162 of 333 and 67 of 158, so F# remains the weakest language that is not text-only. The [record](verification/2026-11-13/README.md#f-and-groovy-with-the-source-mask) has the tables.
- **C and C++ with `#if` through a declaration or an initialiser list** lose every definition after it (`fmt/core.h`, `leveldb/util/env_posix.cc`; the C and C++ section above). The guard refuses a mask that would blank most of a file (`fmt`'s `test/enum-test.cc` went from 23 definitions to 1 before it).
- **A TSX arrow function followed by a ternary on the next line** is read as a syntax error: 4 chart files in tremor, 4 definitions missed.
- **A Scala method dropped without a message.** `SubProcess.joinPumperThreadsHook` (os-lib, `private[os] def`) was extracted in round 1 and is not in round 2.
- **PHP names written from the global namespace.** A supertype `extends \DateTimeImmutable` is rooted since commit `aba5f47` and the edge to a monolog class of that name is gone. Calls and `new` written with a leading backslash are still recorded with an opaque receiver.
- **Kotlin secondary constructors are not extracted** (commit `0127387`).
- **"Receiver named like the type declares it"** still misfires. In C# it measured 9 right of 21 on serilog and moved to `low`, where its edges are still wrong. In Ruby a stdlib `URI::Parser` bound to `Multipart::Parser.parse` at `high` in round 2.
- **Plain `.txt` files are not text-searchable;** a `.txt` file without Vim help markers has no grammar (commit `2695151`).
- **`get_task_context` is slow on a very large repository:** 17.3 s on VS Code at build `a75b887`, before the stored coverage figures (`6ba89a1`) and the answer indexes (`5c77f65`). On the final build a new process takes 7.5 s on VS Code, 14 s on Elasticsearch and 31 s on the Linux kernel; a running server keeps the catalogue in memory.
- **Very large repositories index and resolve slowly, and the Linux kernel is read poorly.** VS Code indexes in 580 s, Elasticsearch in 942 s and resolves in 2,088 s, the Linux kernel indexes in 1,538 s with syntax errors in 35,894 of its 84,603 files. The [record](verification/2026-11-13/README.md#large-repositories-on-the-final-build) has the table and what was not investigated.
- **Eight languages have no definition query on purpose:** Perl, Haskell, Pascal, COBOL, Elm, Nix, Verilog and Assembly. `repocortex languages --verbose` lists each under text only: searchable, with no symbols.
- **Platforms and assets.** `install.ps1` has not been run on Windows (commit `d28e6a4`); no result of the test suite on Windows or Linux for the work in this document is in the record. The demo images in `docs/assets` were last regenerated at v1.0.1 and the portal screenshots on 11 November, before these changes.

## Ruby, C and C++ against compiler indexes (2026-11-18)

Until this section Ruby, C and C++ had been measured only on fixtures. This is
the first measurement on real code against a compiler index, and what it found
and changed. Every figure below is `repocortex accuracy`, run as in "Method":
`index`, `resolve --no-scip`, `scip import`, `accuracy`, on the same corpus
before and after.

### The corpora and the compiler indexes

| Corpus | Language | Files | Index | How the index was made |
|---|---|---|---|---|
| Ruby standard library, `lib/` of 4.0.9 | Ruby | 886 | `scip-ruby` 0.5.1 | run over the tree; it needs no build |
| Homebrew `Library/Homebrew` | Ruby | 2,397 | `scip-ruby` 0.5.1 | the same |
| `pg_query_go` (libpg_query's C) | C | 519 | `scip-clang` 0.4.1 | 74 translation units |
| `codebase-memory-mcp` | C and C++ | 625 (615 C, 10 C++) | `scip-clang` 0.4.1 | 131 translation units |
| `greenlet` | C++ | 53 | `scip-clang` 0.4.1 | 14 translation units |

`scip-clang` reads a compilation database. None of the three C and C++
projects ships one, so each was made for the occasion, and what each contains is
part of what the figures describe:

- **pg_query_go:** written by hand, one entry per `.c` file the Go package's
  `build_cgo.go` compiles, with that file's flags (`-Iinclude -Iinclude/postgres
  -std=gnu99 -DXXH_NAMESPACE=PG_QUERY_`).
- **codebase-memory-mcp:** from `make -n` on the project's Makefile, each compile
  line of the dry run turned into an entry, so the flags are the project's own
  and the tree is the one the project builds for its host (macOS, POSIX).
- **greenlet:** hand-written, `clang++ -std=c++11` with the CPython 3.13 include
  path, for the 14 `.cpp` files the extension compiles.

A file the database does not list has no compiler facts, which is the first
reason coverage is low (below).

### Before and after

Precision is right / wrong edges at that tier over the call sites the compiler
resolved to exactly one definition. Recall is the share of those call sites with
a correct edge at that tier. "Before" is `dev` at `ab925ce`; "after" is this
change.

| Corpus | Tier | Before | After |
|---|---|---|---|
| Ruby standard library 4.0.9 (886 files) | high | 6,651 / 48 (99.3%, recall 62.3%) | 6,479 / 8 (99.9%, recall 60.7%) |
| Ruby standard library 4.0.9 (886 files) | medium | 57 / 12 (82.6%, recall 0.5%) | 57 / 12 (82.6%, recall 0.5%) |
| Ruby standard library 4.0.9 (886 files) | low | 542 / 1,070 (33.6%, recall 5.1%) | 730 / 1,094 (40.0%, recall 6.8%) |
| Ruby standard library 4.0.9 (886 files) | all | 86.5% P, 67.9% R | 86.7% P, 68.0% R |
| Homebrew/brew Library/Homebrew | high | 9,099 / 445 (95.3%, recall 37.5%) | 8,122 / 3 (100.0%, recall 33.4%) |
| Homebrew/brew Library/Homebrew | medium | 1,636 / 134 (92.4%, recall 6.7%) | 1,636 / 134 (92.4%, recall 6.7%) |
| Homebrew/brew Library/Homebrew | low | 4,089 / 212 (95.1%, recall 16.8%) | 5,080 / 640 (88.8%, recall 20.9%) |
| Homebrew/brew Library/Homebrew | all | 94.9% P, 61.0% R | 95.0% P, 61.1% R |
| pg_query_go (C) | high | 4,391 / 22 (99.5%, recall 51.6%) | 4,248 / 1 (100.0%, recall 49.9%) |
| pg_query_go (C) | medium | 10 / 4 (71.4%, recall 0.1%) | 0 / 0 (-) |
| pg_query_go (C) | low | 734 / 0 (100.0%, recall 8.6%) | 887 / 25 (97.3%, recall 10.4%) |
| pg_query_go (C) | all | 99.5% P, 60.4% R | 99.5% P, 60.4% R |
| codebase-memory-mcp (C) | high | 27,979 / 512 (98.2%, recall 74.5%) | 26,837 / 442 (98.4%, recall 71.4%) |
| codebase-memory-mcp (C) | medium | 67 / 1,018 (6.2%, recall 0.2%) | 0 / 0 (-) |
| codebase-memory-mcp (C) | low | 2,265 / 4 (99.8%, recall 6.0%) | 3,474 / 1,092 (76.1%, recall 9.2%) |
| codebase-memory-mcp (C) | all | 95.2% P, 80.7% R | 95.2% P, 80.7% R |
| codebase-memory-mcp (C++) | high | 224 / 3 (98.7%, recall 50.3%) | 224 / 0 (100.0%, recall 50.3%) |
| codebase-memory-mcp (C++) | medium | 20 / 0 (100.0%, recall 4.5%) | 20 / 0 (100.0%, recall 4.5%) |
| codebase-memory-mcp (C++) | low | 10 / 0 (100.0%, recall 2.2%) | 10 / 3 (76.9%, recall 2.2%) |
| codebase-memory-mcp (C++) | all | 98.8% P, 57.1% R | 98.8% P, 57.1% R |
| greenlet (C++) | high | 125 / 16 (88.7%, recall 36.3%) | 141 / 0 (100.0%, recall 41.0%) |
| greenlet (C++) | medium | 9 / 0 (100.0%, recall 2.6%) | 9 / 0 (100.0%, recall 2.6%) |
| greenlet (C++) | low | 64 / 13 (83.1%, recall 18.6%) | 68 / 9 (88.3%, recall 19.8%) |
| greenlet (C++) | all | 87.2% P, 57.6% R | 96.0% P, 63.4% R |

The `all` rows do not move for the languages the changes were about: a binding
that was wrong at `high` is now reported at `low`, where it is still reported.
What moved is which tier it is in. The `high` tier loses recall for it
(Homebrew 37.5% to 33.4%, codebase-memory-mcp 74.5% to 71.4%) and gains the
precision the tier is read for.

### What each change was, and what it measured

**Ruby, "a method of this name in a required file."** A bare call that nothing
in the enclosing type or file declares is a DSL call in a block (`conflicts`,
`depends_on`, `resource`, `desc`, `gem`), and the rule bound it to whichever
required file defined the name, at `high`. On Homebrew it was 956 right and 376
wrong at `high` (71.8%); on the standard library, where the name is a module's own
helper, 93%. Capped at `medium` it was 2 right of 378, so it is `low`. On
Homebrew it is still 2 right of 501 at `low`, and the rule is the largest
single contributor to Homebrew's `low` tier falling from 95.1% to 88.8%. That is
the cost of reporting it at all; it is not a rule to trust.

**Ruby, a same-file `def` that shadows a DSL method.** A spec-local `def patch`
bound the `patch do ... end` that runs against `Formula`. Whether the call
passes a block is now recorded on the reference (`symbol_references.flags`
bit 1, migration 0028, read for Ruby only). A call that passes a block, where the
same name is a method of a class in a file that is not a test, is `low`: capped at
`medium` it was 0 right of 23. The same name defined twice at one level of a file
(rspec helpers in separate `describe` blocks) is `low`, 0 right of 27. The
helper called without a block stays `high`. Homebrew's `high` wrong from this
rule fell from 52 to 2.

**Ruby, a class method and an instance method of one name.** `def self.x` and
`def x` share a qualified name. Whether a Ruby definition is a singleton
(`def self.x`, `def Const.x`, or under `class << self`) is now a bit of
`symbols.flags` (16), and a bare call or a call on `self` prefers the method of
the kind of the `def` it is written in. Where only the other kind exists the
binding is `low`: 57 call sites on the standard library, none right. The
standard library's `high` wrong fell from 48 to 8 (`each_address` in `resolv.rb` and
`start` in `net/http.rb` were two of the ones that were the other kind).

**Ruby, a method defined twice in a type.** "Declared on the enclosing type" took
the first of several same-name, same-kind members at `high`. It is now `low`:
capped at `medium` it was 0 right of 12 on the standard library. One of each
kind is not a redefinition.

**C and C++, preprocessor alternatives.** `compat_fs.c` defines `cbm_fopen`
once under `#ifdef _WIN32` and once for POSIX; a binding to either was `high` (the
same file, the first declaration) or `medium` (several matches among imports),
and `medium` was 67 right of 1,085 (6.2%) on codebase-memory-mcp, 976 of the
wrong ones variants of one function. Whether a C or C++ definition sits in a
conditional branch is now a bit of `symbols.flags` (4, the same bit as Rust's `#[cfg]`), read from the directives
in the text with the scanner `cond.go` already has, because the masked retry
parse blanks them. A conditional that is an include guard, or names
`__cplusplus`, is not one. A call is `low` where any candidate of the name is
conditional, where one file defines a C name twice, or where the binding is
"multiple matches among imports". C++ overloads are not alternatives. Result:
codebase-memory-mcp's `medium` tier is empty (it was 1,085 call sites) and its
`high` tier lost 3.1 points of recall to `low`, whose precision is now 76.1% over
4,566 call sites instead of 99.8% over 2,269. pg_query_go's 22 wrong `high`
edges became 1, and 21 of them are `low` now. Its one that remains is a
`static` function the compiler index attributes to another file, below.

**C++ INSTANTIATES.** `Foo(...)` and `new Foo` bind `INSTANTIATES` to the class;
`scip-clang` binds the constructor, so all 16 wrong `high` edges on greenlet were
this. The edge stays on the class, as it is for Java, C#, Kotlin, Python and
Ruby: a class may declare no constructor, or several that the argument count does
not tell apart, and a compiler index that has constructors is the exception, not
the rule. Binding to the constructor where one is indexed would give C++ a
different `INSTANTIATES` target from every other language. The adjudication takes the other
side of the difference instead: for an `INSTANTIATES` edge, the type and its own
constructor are the same answer (`registry/accuracy.go`, `sameAnswer`).
The constructor is a method or function of the same name whose qualified name is
the type's followed by `.Name`; where it is written outside the header that
declares the class the type must be the only one of that qualified name, or one
class's constructor would vouch for another's. A `CALLS` edge to the type, an
edge to another class of the same name, and an edge to a method that is not the
constructor are still different answers. The change is to the measurement, not
to the graph, and it is not specific to C++: the rule reads names and qualified
names, and Java and C# were not measured here. greenlet: 16 wrong at `high` to 0,
141 right. An edge to a type is also never lowered for a constructor written
under an `#if`, which is a variant of the constructor and not of the type.

### Caveats

- **Coverage is 15% to 37% of call sites.** The adjudicated sites are those the
  compiler resolved to one definition on a line where the parser also found a
  call: 10,680 of 47,116 (22.7%) on the standard library, 24,296 of 162,050
  (15.0%) on Homebrew, 8,505 of 30,234 (28.1%) on pg_query_go, 38,516 of 201,524
  (19.1%) on codebase-memory-mcp, 344 of 940 (36.6%) on greenlet. The rest is
  lines with no compiler fact, lines with several, and files the compilation
  database did not list. These are rates over that slice, as the report says.
- **`scip-clang` conflates `static`.** Two `static` functions of one name in two
  files are two functions; the index attaches a call in one file to the other's
  in places. Of the 442 `high` edges still wrong on codebase-memory-mcp, 256 bind
  a `static` function in the call's own file (`heap_strdup` alone is 204): the
  edge the language requires, scored as wrong against the index. That counts
  against RepoCortex in every C figure above and is not a defect in it. The other
  186 are the case this change did not try to fix: two real non-static
  definitions in different directories, which the link target decides
  (`cbm_arena_alloc` in `internal/cbm/arena.c` and `src/foundation/arena.c`).
- **The C++ samples are small.** codebase-memory-mcp has 10 C++ files and 445
  adjudicated sites, greenlet 344 sites in 30 files. A 100% on 141 sites is a
  statement about greenlet, not about C++.
- **The corpora are what they are.** Homebrew and the standard library are
  Ruby written with a great deal of metaprogramming, and one project each is not
  all of Ruby. The compilation databases were made by hand or by a dry run, and a host
  with another platform's `#ifdef` branches would resolve to the other variants.
- **Recall from the `all` rows did not change.** The changes move edges between
  tiers; they do not remove any, and the call sites with no edge at all are
  the same.
- **`high` recall did drop,** and the cost is real: `high` holds 977 fewer right
  edges on Homebrew, 1,142 fewer on codebase-memory-mcp and 143 fewer on
  pg_query_go than before. They are reported one or two tiers down, not lost.
  Whether that is the right price for the precision is a judgement; the
  measurements above are what it is made on.

## Rust against rust-analyzer (2026-11-19)

Rust was measured against `rust-analyzer scip` 1.99.1 with the method of this document: `index --full --accuracy fast`, `resolve --no-scip`, `scip import`, `accuracy`, and the wrong and missed call sites read edge by edge from the index (the pre-import copy gives each rule's own precision).

**Corpora.** Three crates with no network: rtk (`rtk-develop`, 130 files, a command-line tool, 1,639 adjudicated sites), regex-syntax 0.8.11 (1,511 sites) and sqlparser 0.53.0 (2,966 sites). Each is one crate indexed with `CARGO_NET_OFFLINE=true`.

| `high`, raw | Before: right / judged | Before: precision, recall | After: right / judged | After: precision, recall |
|---|---:|---:|---:|---:|
| rtk | 1,467 / 1,539 | 95.3%, 87.4% | 1,531 / 1,603 | 95.5%, 91.2% |
| regex-syntax | 1,440 / 1,456 | 98.9%, 87.1% | 1,446 / 1,462 | 98.9%, 87.4% |
| sqlparser | 2,869 / 2,873 | 99.9%, 90.3% | 2,876 / 2,876 | 100.0%, 90.5% |

| `medium`, raw | Before | After |
|---|---:|---:|
| rtk | 46 / 49 (93.9%) | 36 / 36 (100%) |
| regex-syntax | 42 / 44 (95.5%) | 36 / 36 (100%) |
| sqlparser | 70 / 73 (95.9%) | 64 / 68 (94.1%) |

Recall over all tiers: rtk 90.1% to 93.3%, regex-syntax 90.0% to 90.3%, sqlparser 93.1% unchanged.

**The caveat: raw precision understates RepoCortex.** `rust-analyzer scip` names a private item by its path, and two same-named private items that rust-analyzer cannot tell apart collapse to one symbol: the `tests/*.rs` files of one crate (each is its own crate, and a `fn helper` in two of them is one symbol), and functions nested in functions (`fn imp` inside `perl_word` and inside `sb` is one `sb.<locals>.imp`). A call that RepoCortex binds to the nested function it is written in is then judged wrong. Every one of the `high` disagreements was read:

| `high` | Raw wrong | Of them, rust-analyzer collapsing same-named items | Real wrong | Adjusted precision |
|---|---:|---:|---:|---:|
| rtk | 72 | 72 | 0 | 1,531 / 1,531 |
| regex-syntax | 16 | 16 | 0 | 1,446 / 1,446 |
| sqlparser | 0 | 0 | 0 | 2,876 / 2,876 |

The adjusted figure is the one a user would see against a compiler that kept the items apart; the raw one is what the tool reports, and is the one the bar is set on (95% on every corpus).

**What changed, and what each fix did on these corpora.**

1. **`use` lists, renames and globs are stored, and read.** `use a::{b, c::d}`, `use a::b as c`, `{self}` and `use a::*` had no import row (the query matched a plain path only). Each leaf is now a row (`internal/parse/lang_rust_uses.go`), and a bare call is bound to the module a `use` names, a module being `<src>/a/b.rs` or `<src>/a/b/mod.rs` (`infer/rustuse.go`). A path that starts `core::`, `std::` or `alloc::` beside a `mod core;` is that module and not the standard library, and `utils::f()` after `use crate::core::utils;` is the function of that file, not of any file called `utils`. `self::` and `super::` are not placed (an inline `mod tests` makes the file's path wrong about them). Measured on rtk: `high` right 1,467 to 1,537 (recall 87.4% to 91.5%), wrong unchanged at 72, `medium` wrong 3 to 0 (the three "same directory, but the language requires an import" edges, `gain.rs:488` among them, now bind to `core/display_helpers.rs`), 91 missed sites to 40 (`resolved_command` 9 to 0, `decode_process_output` 4 to 0, `truncate` 3 to 0). regex-syntax: 1,440 to 1,446 right. sqlparser: 2,869 to 2,876. No `high` edge was added that is wrong.
2. **Definitions under `#[cfg(...)]`.** The parser records that a Rust item, or an item enclosing it, has a `cfg` attribute (`symbols.flags`, bit 4, no migration). A call that binds `high` to a conditional definition with a conditional twin of the same qualified name is held at `medium` and says why (`infer/rustcfg.go`). sqlparser's `try_decrease` at `parser/mod.rs:95` and `:138` (`mod recursion` for `std` and for `not(std)`): 3 wrong `high` edges, now `medium`. rtk: 7 edges to `#[cfg(unix)]` / `#[cfg(not(unix))]` twins (`set_owner_only`, `output_codepage`), all right, move from `high` to `medium`, which is why rtk's `high` recall is 91.2% and not 91.5%. A function nested in the function the call is written in is exempt, since its twin is the other alternative's own.
3. **One trait for `T` and for `&T`.** A method of an `impl Trait for &T` is marked (bit 8), and a `high` binding to a method that has a same-named twin differing in that mark is held at `medium` (`ours.into_iter()` on an owned value bound to `impl<'a, T> IntoIterator for &'a T`, `ast/mod.rs:7911`). The receiver is not read to choose between the two: the source rarely says, and the cap is the honest answer where it does not.
4. **A variable's name against what it was assigned.** `let mut class = self.convert_unicode_class_error(..)?; class.negate()` was bound to `Class.negate` at `medium` because the variable is named `class`; the call that assigned it is declared to return `Result<hir::ClassUnicode>`. Where the call that assigned a variable declares a return type that is not a type of the repository, the name no longer decides (`infer/rustnamed.go`). The per-rule figures, from the pre-import index: "receiver is named after the type that declares it" on a path or scope receiver 4 of 4 at `high`; on a name no declaration binds 35 of 35 at `medium`; on a variable declared without a type 10 of 15 at `medium` (regex-syntax 6 of 8, sqlparser 4 of 7). The last is now held at `low`. `NamedAfterType: High` stays for Rust: it applies only to the receivers that measured 4 of 4, and the weak case was the one capped by `namedAfterTypeIsHint`. Measured: `medium` wrong on regex-syntax 2 to 0 and on sqlparser 3 (`query.span()` on a `Select`) to 0 of that kind.
5. **Not done:** closure parameter types from iterator adapters (`.map(|i| i.span())`, 83 missed sites in sqlparser) and enum tuple-variant constructors (`ParserError::ParserError(..)`, 31). Both need a type for a binding the parser does not read, and neither could be shown not to add a wrong `high` edge in the time.

The `high` bar (95% raw on every corpus) holds on all three, with rtk the narrowest at 95.5%: its 72 are all the collapse above, and the margin is that arithmetic and not a loosened rule.

## Reproducing

The numbers above came from:

| | |
|---|---|
| Host | Apple M4 Pro, macOS 26.5.1, `darwin/arm64` native build |
| RepoCortex | commit at the time of writing, `CGO_ENABLED=0` |
| Ground truth | `scip-go` 0.2.7, `scip-typescript` 0.3.14, `scip-python` 0.6.6, `scip-java` `HEAD-20260402-1456` (pinned image digest `a05564b9…`, docker runner) |
| Corpora | `--depth 1` clones: prometheus, nest, scrapy, requests, spring-petclinic-rest (`4cd8e1b`) |
| Python scope analysis | `--depth 1` clones of scrapy `c7eacdbb30a4563bbbe04ed02781768f4e19eabb`, requests `v2.32.3` (`0e322af8`), httpx `0.28.1` (`26d48e06`), django `5.1.4` (`2d4add11`); `scip-python index --project-name repocortex-repository --project-version 0.0.0`, as `repocortex indexers` runs it; `index --full --accuracy fast`, `resolve --no-scip`, `scip import`, `accuracy` |

`scip-go` on prometheus takes about eight minutes and produces a 57 MB index.
On RepoCortex's own source it takes nineteen seconds and produces 7 MB.
