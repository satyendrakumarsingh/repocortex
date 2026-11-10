## Description
<!-- Briefly describe the changes introduced by this pull request and the rationale. -->

## Type of Change
- [ ] 🐛 Bug fix (non-breaking change fixing an unexpected defect)
- [ ] ✨ New feature or capability (non-breaking enhancement)
- [ ] ⚡ Performance improvement / latency optimization
- [ ] 🛡️ Security hardening / path containment / redaction update
- [ ] 🏗️ Architecture invariant / schema migration
- [ ] 📝 Documentation / examples / guides

## Changes Introduced
- 

## Core Invariants Checklist
- [ ] **Zero-CGO Pure-Go Guarantee**: Changes do not introduce C toolchain dependencies (`CGO_ENABLED=0` remains functional).
- [ ] **Provenance & Confidence**: Graph edges assign accurate `Confidence` (`exact`, `high`, `medium`, `low`) and authoritative `Evidence`.
- [ ] **Containment & Redaction**: Untrusted paths are validated against symlink/directory escapes, and secrets never enter the index or telemetry.
- [ ] **Bounded Outputs**: Agent tools and API outputs enforce size caps (no unbounded file reads or memory growth).

## Verification & Testing
<!-- Describe how you verified these changes. Include commands and test outputs. -->
- [ ] Ran `go test -race ./...` and all tests pass.
- [ ] Ran `gofmt -l .` and no formatting diffs exist.
- [ ] Ran `go vet ./...` with zero issues.
- [ ] Verified backwards compatibility with existing `.cortex` databases/indexes (or added schema migration).
- [ ] Updated user guides, CLI reference, or agent skills if adding new flags, commands, or behaviors.

## Related Issues & Discussions
<!-- Reference any related issue(s): Fixes #123, Resolves #456 -->
