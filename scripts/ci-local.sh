#!/usr/bin/env bash
#
# ci-local.sh — run everything .github/workflows/ci.yml runs, locally.
#
# The workflows are the contract; this script is the same contract in a form a
# developer can run before pushing. Every step below mirrors a step in ci.yml,
# in the same order and with the same command line, so a green run here means
# the same thing a green run there does. When you change one, change the other:
# a local script that has quietly drifted from CI is worse than no script,
# because it buys confidence it cannot back.
#
# What it deliberately does NOT reproduce:
#   * the three-OS matrix — you have one OS. The cross-compile step covers the
#     build half of that; the Windows/macOS test half only exists on CI.
#   * the nightly fuzz and soak jobs — see --soak / --fuzz to opt in.
#   * the CGO differential job in cgo-backend.yml — it needs a C toolchain and
#     several minutes of grammar compilation. See --cgo-parser.
#
# Usage:
#   scripts/ci-local.sh [--quick] [--soak] [--fuzz] [--cgo-parser] [--list]
#
#   --quick       Skip the slow matrix: the five-target cross-compile, the
#                 CGO_ENABLED=0 test run, and govulncheck. Keeps every gate
#                 that can fail on a one-line change.
#   --soak        Also run the endurance suite (nightly on CI). Minutes.
#   --fuzz        Also run the fuzz gate (nightly on CI). Minutes.
#   --cgo-parser  Also run the differential parser gate from cgo-backend.yml.
#                 Needs CGO and a C compiler.
#   --list        Print the steps that would run, then exit.
#
# Exits non-zero on the FIRST failing step. It does not collect failures and
# report at the end: the first thing that broke is the thing to fix, and a wall
# of consequent failures buries it.

set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

# Keep these in lockstep with `env:` in .github/workflows/ci.yml.
GO_VERSION="1.26.10"
GOLANGCI_LINT_VERSION="v2.15.0"
TEST_TIMEOUT="20m"

QUICK=0
RUN_SOAK=0
RUN_FUZZ=0
RUN_CGO_PARSER=0
LIST_ONLY=0

for arg in "$@"; do
  case "$arg" in
    --quick)      QUICK=1 ;;
    --soak)       RUN_SOAK=1 ;;
    --fuzz)       RUN_FUZZ=1 ;;
    --cgo-parser) RUN_CGO_PARSER=1 ;;
    --list)       LIST_ONLY=1 ;;
    -h|--help)    sed -n '2,40p' "${BASH_SOURCE[0]}" | sed 's|^# \{0,1\}||'; exit 0 ;;
    *)
      printf 'ci-local.sh: unknown option %s (try --help)\n' "$arg" >&2
      exit 2
      ;;
  esac
done

if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  C_BOLD=$'\033[1m'; C_RED=$'\033[31m'; C_GREEN=$'\033[32m'
  C_DIM=$'\033[2m';  C_RESET=$'\033[0m'
else
  C_BOLD=''; C_RED=''; C_GREEN=''; C_DIM=''; C_RESET=''
fi

step_no=0
started_at=$(date +%s)
current_step=""

banner() {
  step_no=$((step_no + 1))
  current_step="$1"
  printf '\n%s══════════════════════════════════════════════════════════════%s\n' "$C_BOLD" "$C_RESET"
  printf '%s  [%02d] %s%s\n' "$C_BOLD" "$step_no" "$1" "$C_RESET"
  printf '%s       job: %s%s\n' "$C_DIM" "$2" "$C_RESET"
  printf '%s══════════════════════════════════════════════════════════════%s\n' "$C_BOLD" "$C_RESET"
}

# Reports which step died. Without this, `set -e` exits silently and you are
# left scrolling to work out which of twenty commands actually failed.
on_failure() {
  local code=$?
  printf '\n%s✗ FAILED%s at step %02d: %s%s%s\n' \
    "$C_RED$C_BOLD" "$C_RESET" "$step_no" "$C_BOLD" "$current_step" "$C_RESET" >&2
  printf '%s  exit code %d after %ds%s\n' "$C_DIM" "$code" "$(( $(date +%s) - started_at ))" "$C_RESET" >&2
  exit "$code"
}
trap on_failure ERR

if [ "$LIST_ONLY" = "1" ]; then
  cat <<'EOF'
lint      gofmt
lint      go vet ./...
lint      go vet -tags soak ./tests/soak/...
lint      golangci-lint run ./...
lint      go mod tidy && git diff --exit-code go.mod go.sum
test      go build ./...
test      go test -race -timeout 20m ./...
test      go test -race -v -run TestFTS5 ./tests/integration/...
cgo-free  cross-compile 5 targets at CGO_ENABLED=0   (skipped by --quick)
cgo-free  CGO_ENABLED=0 go test ./...                (skipped by --quick)
security  go test -run TestRepoCannot ./internal/config/... ./internal/indexer/...
security  go test -run TestDoctorDetects ./internal/cli/...
security  go test -run 'TestSecrets|TestRedaction' ./tests/integration/...
security  go test -race ./internal/security/...
security  govulncheck ./...                          (skipped by --quick)
soak      go test -tags soak ./tests/soak/...        (--soak only)
fuzz      go test -fuzz=FuzzSplitIdentifier          (--fuzz only)
cgo       differential parser gate                   (--cgo-parser only)
EOF
  exit 0
fi

# ---------------------------------------------------------------------------
# Preflight
# ---------------------------------------------------------------------------
banner "preflight" "n/a"
have_go=$(go version)
printf '  %s\n' "$have_go"
if ! printf '%s' "$have_go" | grep -q "go$GO_VERSION"; then
  printf '  %s! local Go differs from CI (ci.yml pins %s)%s\n' "$C_DIM" "$GO_VERSION" "$C_RESET"
fi
printf '  repo: %s\n' "$repo_root"
[ "$QUICK" = "1" ] && printf '  %smode: --quick (slow matrix skipped)%s\n' "$C_DIM" "$C_RESET"

# ---------------------------------------------------------------------------
# Job: lint  —  "format / vet / lint"
# ---------------------------------------------------------------------------
banner "gofmt" "lint"
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
  printf '%sgofmt required for:%s\n%s\n' "$C_RED" "$C_RESET" "$unformatted" >&2
  exit 1
fi
printf '  all files formatted\n'

banner "go vet ./..." "lint"
go vet ./...

banner "go vet -tags soak" "lint"
go vet -tags soak ./tests/soak/...

banner "golangci-lint run ./..." "lint"
if ! command -v golangci-lint >/dev/null 2>&1; then
  printf '%sgolangci-lint not installed. CI pins %s:%s\n' "$C_RED" "$GOLANGCI_LINT_VERSION" "$C_RESET" >&2
  printf '  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@%s\n' "$GOLANGCI_LINT_VERSION" >&2
  exit 1
fi
# .golangci.yml is `version: "2"`. golangci-lint v1 cannot read it at all, and
# says so in a way that is easy to misread as a config bug rather than a
# version mismatch — which is exactly how CI's lint job was broken.
have_lint=$(golangci-lint version 2>&1 || true)
printf '  %s\n' "$have_lint"
case "$have_lint" in
  *"version v1."*|*"version 1."*)
    printf '%sgolangci-lint v1 cannot read this repo'"'"'s version: "2" config.%s\n' "$C_RED" "$C_RESET" >&2
    printf '  install %s instead.\n' "$GOLANGCI_LINT_VERSION" >&2
    exit 1
    ;;
esac
golangci-lint run ./...
printf '  0 issues\n'

banner "go.mod is tidy" "lint"
# CI can afford `go mod tidy && git diff --exit-code` because its checkout is
# disposable. Yours is not: run that here and a stale go.sum silently becomes
# an edit in your working tree, which is how the "fix" ends up in an unrelated
# commit. So tidy against a copy and restore unconditionally — the backups live
# outside the repo so an interrupted run cannot leave litter in it either.
mod_backup=$(mktemp -d)
cp go.mod go.sum "$mod_backup/"
restore_mod() {
  if [ -f "$mod_backup/go.mod" ]; then
    cp -f "$mod_backup/go.mod" "$repo_root/go.mod"
    cp -f "$mod_backup/go.sum" "$repo_root/go.sum"
  fi
  rm -rf "$mod_backup"
}
trap 'restore_mod; on_failure' ERR
trap 'restore_mod' EXIT INT TERM
go mod tidy
tidy_diff=0
diff -q "$mod_backup/go.mod" go.mod >/dev/null 2>&1 || tidy_diff=1
diff -q "$mod_backup/go.sum" go.sum >/dev/null 2>&1 || tidy_diff=1
if [ "$tidy_diff" = "1" ]; then
  printf '%sgo mod tidy changed go.mod/go.sum — commit the result:%s\n' "$C_RED" "$C_RESET" >&2
  diff -u "$mod_backup/go.mod" go.mod || true
  diff -u "$mod_backup/go.sum" go.sum || true
  exit 1
fi
restore_mod
trap on_failure ERR
trap - EXIT INT TERM
printf '  go.mod and go.sum are tidy\n'

# ---------------------------------------------------------------------------
# Job: test  —  "test (ubuntu|macos|windows)"
#
# CI runs this on three operating systems. You are running it on one. The
# platform-specific failures this suite is most exposed to — symlink creation
# needing elevation on Windows, chmod not denying reads there — are handled by
# runtime.GOOS skips in the tests themselves, so a green run here does not
# prove the other two are green.
# ---------------------------------------------------------------------------
banner "go build ./..." "test"
go build ./...

banner "go test -race -timeout $TEST_TIMEOUT ./..." "test"
go test -race -timeout "$TEST_TIMEOUT" ./...

banner "FTS5 dependency guarantee" "test"
# Verifies an undocumented property of modernc.org/sqlite: that the FTS5
# extension is compiled into the pure-Go build. If this fails, search is broken
# even though everything compiles.
go test -race -v -run 'TestFTS5' ./tests/integration/...

# ---------------------------------------------------------------------------
# Job: cgo-free  —  "builds without CGO"
# ---------------------------------------------------------------------------
if [ "$QUICK" = "1" ]; then
  banner "cross-compile 5 targets (SKIPPED: --quick)" "cgo-free"
  printf '  %sskipped%s\n' "$C_DIM" "$C_RESET"
else
  banner "cross-compile 5 targets at CGO_ENABLED=0" "cgo-free"
  for target in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64; do
    os="${target%/*}"; arch="${target#*/}"
    printf '  --- %s/%s\n' "$os" "$arch"
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -o /dev/null ./cmd/cortex
  done
  printf '  all five targets build with no C toolchain\n'

  banner "CGO_ENABLED=0 go test ./..." "cgo-free"
  CGO_ENABLED=0 go test -timeout "$TEST_TIMEOUT" ./...
fi

# ---------------------------------------------------------------------------
# Job: security  —  "security gates"
# ---------------------------------------------------------------------------
banner "repo config cannot grant execution" "security"
go test -race -v -run 'TestRepoCannot' ./internal/config/... ./internal/indexer/...

banner "graph invariant gates fail on corruption" "security"
go test -race -v -run 'TestDoctorDetects' ./internal/cli/...

banner "secrets never leave the repository" "security"
go test -race -v -run 'TestSecrets|TestRedaction' ./tests/integration/...

banner "redaction and path-containment engine" "security"
go test -race ./internal/security/...

if [ "$QUICK" = "1" ]; then
  banner "govulncheck (SKIPPED: --quick)" "security"
  printf '  %sskipped%s\n' "$C_DIM" "$C_RESET"
else
  banner "govulncheck ./..." "security"
  if ! command -v govulncheck >/dev/null 2>&1; then
    printf '  govulncheck not installed; installing…\n'
    go install golang.org/x/vuln/cmd/govulncheck@latest
  fi
  govulncheck_bin=$(command -v govulncheck || printf '%s/bin/govulncheck' "$(go env GOPATH)")
  "$govulncheck_bin" ./...
fi

# ---------------------------------------------------------------------------
# Nightly jobs — opt in.
# ---------------------------------------------------------------------------
if [ "$RUN_SOAK" = "1" ]; then
  banner "soak: the build tag hides these by default" "soak"
  untagged=$(go list -f '{{ len .TestGoFiles }}' ./tests/soak/)
  tagged=$(go list -tags soak -f '{{ len .TestGoFiles }}' ./tests/soak/)
  printf '  test files: untagged=%s tagged=%s\n' "$untagged" "$tagged"
  if [ "$untagged" != "0" ] || [ "$tagged" = "0" ]; then
    printf '%sthe soak build tag is not doing its job%s\n' "$C_RED" "$C_RESET" >&2
    exit 1
  fi

  banner "soak: endurance suite" "soak"
  go test -tags soak -race -timeout 45m ./tests/soak/... -soak.duration="${SOAK_DURATION:-2m}"
fi

if [ "$RUN_FUZZ" = "1" ]; then
  banner "fuzz identifier splitter" "fuzz"
  go test -run=NONE -fuzz=FuzzSplitIdentifier -fuzztime="${FUZZTIME:-1m}" ./internal/search/
fi

if [ "$RUN_CGO_PARSER" = "1" ]; then
  banner "cgo-backend: default build is still CGO-free" "differential"
  CGO_ENABLED=0 go build ./internal/parse/...
  CGO_ENABLED=0 go test -count=1 ./internal/parse/...

  banner "cgo-backend: build and vet the CGO backend" "differential"
  CGO_ENABLED=1 go build -tags cgo_parser ./internal/parse/...
  CGO_ENABLED=1 go vet -tags cgo_parser ./internal/parse/...

  banner "cgo-backend: differential comparison" "differential"
  CGO_ENABLED=1 go test -tags cgo_parser -race -count=1 -timeout 20m -v ./internal/parse/
fi

# ---------------------------------------------------------------------------
elapsed=$(( $(date +%s) - started_at ))
printf '\n%s✓ all %d steps passed%s in %dm%02ds\n' \
  "$C_GREEN$C_BOLD" "$step_no" "$C_RESET" "$((elapsed / 60))" "$((elapsed % 60))"
if [ "$QUICK" = "1" ]; then
  printf '%s  --quick was set: the cross-compile matrix, the CGO_ENABLED=0 test\n' "$C_DIM"
  printf '  run and govulncheck did NOT run. Run without --quick before pushing.%s\n' "$C_RESET"
fi
printf '%s  Not covered locally: the macOS and Windows halves of the test matrix.%s\n' "$C_DIM" "$C_RESET"
