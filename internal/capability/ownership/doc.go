// Package ownership answers "who has context on the code a change here
// reaches" by joining git history to a blast radius.
//
// # Why reachability rather than directories
//
// CODEOWNERS routes a review by the path a file sits in. That is the right
// answer when the question is "who administers this directory" and the wrong
// answer when the question is "who has seen the code this change can break",
// because a change's blast radius does not respect directories: a two-line edit
// to a shared decoder reaches thirty call sites in six packages, and the person
// listed for the decoder's directory may never have touched any of them.
//
// So this package starts from an impact.Report — the symbols a change to the
// target can actually reach — and asks git which people have touched the files
// those symbols live in. The ranking is by PROPORTION OF THE REACHED SET, not by
// commit count: someone with four hundred commits to an unrelated part of the
// repository outranks nobody here, and someone with three commits that happen to
// span 80% of the blast radius outranks everybody.
//
// # This names people, and the framing is not decoration
//
// Git records that a person changed a file. It does not record that they
// understand it, that they own it, that they still work here, or that they wrote
// the bug. Every label this package produces therefore says the person HAS
// CONTEXT ON the code, and the Report carries the limitations as data
// (Report.Caveats) so that a renderer cannot print the names without printing
// what the names mean. That is deliberate: a list of names under a heading like
// "responsible" is read as an accusation the moment something breaks, and the
// tool that produced it will have supplied the evidence for a conversation it
// has no basis for.
//
// Three limits are stated in every report because a reader who does not know
// them will draw the wrong conclusion from the numbers, not merely a vaguer one:
//
//  1. Attribution is FILE-level, not line-level. A commit touching a 900-line
//     file is credited with every reached symbol in it. Line-level attribution
//     means blame, and blame costs ~2 seconds per file on a 500-commit
//     repository (internal/acquire/git measures it and declines to expose it),
//     which is not a per-file cost an interactive query can pay.
//
//  2. The history window is bounded. Only the newest N commits from HEAD are
//     read, so a person whose work predates the window is invisible — which
//     reads identically to a person who never touched the code.
//
//  3. An identity is a name and an email, not a person. One human committing
//     from two addresses appears twice; a shared or automated account appears as
//     one. Names are reproduced exactly as git recorded them, and every person is
//     referred to as "they", because a commit records a name and no pronoun.
//
// # Determinism
//
// Identical inputs must produce identical output, and the inputs here include a
// commit list whose order this package does not control. Every input is
// therefore re-sorted on entry — commits by author time then SHA, paths within a
// commit, symbols by id — every grouping is built by walking an ordered slice
// rather than by ranging the map it fills, and every ranking key is total. This
// matters more than it looks: the registry digest is the product's foundation,
// and a report that differs between two runs over the same repository cannot be
// diffed, cached, or trusted.
//
// # What this package does not do
//
// It does not open a repository, shell out, or know that go-git exists. History
// arrives through the History port as commits whose paths are already in the
// index's path space; translating a git worktree's paths into that space is the
// caller's job and its error to wrap. See ports.go.
package ownership
