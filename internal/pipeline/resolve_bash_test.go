package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestBashResolution indexes a small set of scripts and their bats tests and
// checks every call against what `source` makes it reach.
//
// What it is built to catch: `log_info` defined in lib/log.sh, which deploy.sh
// sources, and again in lib/legacy.sh, which it does not; a function that
// shares its name with a utility (`cat`, `date`) defined by a script that was
// not sourced; a file sourced two ways (relative to the script and through
// "$SCRIPT_DIR"); a path built from a variable the script cannot know; a
// helper reached by `load` in a bats file; and calls made at a script's top
// level, outside every function.
func TestBashResolution(t *testing.T) {
	got := langEdges(t, bashFixture())
	high := model.ConfidenceHigh
	wants := []want{
		{"scripts/deploy.sh:main", "scripts/lib/log.sh:log_info", high},
		{"scripts/deploy.sh:main", "scripts/lib/net.sh:fetch", high},
		{"scripts/deploy.sh:main", "scripts/deploy.sh:build", high},
		{"scripts/deploy.sh:build", "scripts/lib/log.sh:log_error", high},
		{"scripts/deploy.sh:(top level)", "scripts/deploy.sh:main", high},
		{"scripts/lib/log.sh:die", "scripts/lib/log.sh:log_error", high},
		{"scripts/lib/net.sh:fetch", "scripts/lib/log.sh:log_info", high},
		{"scripts/lib/net.sh:fetch", "scripts/lib/net.sh:retry", high},
		{"scripts/lib/net.sh:retry", "scripts/lib/log.sh:die", high},
		{"scripts/status.sh:report", "scripts/lib/legacy.sh:log_info", high},
		{"scripts/status.sh:report", "scripts/lib/legacy.sh:cleanup", high},
		{"scripts/status.sh:report", "scripts/status.sh:render", high},
		{"test/deploy.bats:(top level)", "scripts/lib/net.sh:fetch", high},
		{"test/deploy.bats:(top level)", "scripts/lib/log.sh:log_info", high},
	}
	forbid := []want{
		// legacy.sh is not sourced by deploy.sh or net.sh.
		{Src: "scripts/deploy.sh", Dst: "scripts/lib/legacy.sh"},
		{Src: "scripts/lib/net.sh", Dst: "scripts/lib/legacy.sh"},
		{Src: "scripts/lib/log.sh", Dst: "scripts/lib/legacy.sh"},
		// status.sh sources legacy.sh only.
		{Src: "scripts/status.sh", Dst: "scripts/lib/log.sh"},
		// `cat` and `date` are the utilities, not legacy.sh's wrappers.
		{Src: "scripts/deploy.sh", Dst: "legacy.sh:cat"},
		{Src: "scripts/lib/net.sh", Dst: "legacy.sh:date"},
		// orphan.sh sources nothing, so neither name reaches a definition.
		{Src: "scripts/orphan.sh", Dst: "scripts/lib/"},
		// A path built from $LIB_DIR names no file: tools.sh is not sourced.
		{Src: "scripts/deploy.sh", Dst: "scripts/lib/tools.sh"},
	}
	scoreEdges(t, got, wants, forbid, nil)
}

func bashFixture() map[string]string {
	return map[string]string{
		"scripts/deploy.sh": `#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

source "$SCRIPT_DIR/lib/log.sh"
. ./lib/net.sh
source "$LIB_DIR/tools.sh"

build() {
  git build
  cat Makefile
  log_error "none" || true
}

main() {
  log_info "start"
  fetch "https://example.com"
  build
  date +%s
}

main "$@"
`,
		"scripts/lib/log.sh": `log_info() {
  echo "[info] $*"
}

log_error() {
  echo "[error] $*" >&2
}

die() {
  log_error "$@"
  exit 1
}
`,
		"scripts/lib/net.sh": `source "${BASH_SOURCE%/*}/log.sh"

retry() {
  for i in 1 2 3; do
    curl -fsS "$1" && return 0
  done
  die "gave up"
}

fetch() {
  log_info "fetching $1"
  retry "$1"
  date
}
`,
		"scripts/lib/legacy.sh": `log_info() {
  echo "legacy: $*"
}

cleanup() {
  rm -rf /tmp/work
}

cat() {
  command cat "$@"
}

date() {
  command date "$@"
}
`,
		// A second lib/net.sh: `. ./lib/net.sh` from scripts/ is scripts/lib/net.sh,
		// the longer match, and not this one.
		"lib/net.sh": `fetch() {
  wget "$1"
}
`,
		"scripts/orphan.sh": `# Sources nothing: what it calls is not in scope.
cleanup
die "no"
`,
		"scripts/lib/tools.sh": `build_all() {
  make all
}
`,
		"scripts/status.sh": `source ./lib/legacy.sh

render() {
  echo "ok"
}

report() {
  log_info "status"
  cleanup
  render
}
`,
		"test/deploy.bats": `load '../scripts/lib/net'
source "$BATS_TEST_DIRNAME/../scripts/lib/log.sh"

@test "fetch logs" {
  run fetch "x"
  log_info "x"
}
`,
	}
}
