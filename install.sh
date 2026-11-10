#!/usr/bin/env sh
# RepoCortex installer for macOS and Linux.
#
# POSIX sh, not bash: this is piped to whatever /bin/sh the user has, and on
# Debian derivatives that is dash. The checksum verification is not optional
# and there is no flag to skip it — an installer that will happily run an
# unverified binary is not worth shipping.
set -eu

REPO="${REPO:-satyendrakumarsingh/repocortex}"
INSTALL_DIR="${HOME}/.local/bin"
VERSION="latest"

usage() {
  cat <<'USAGE'
Usage: install.sh [--dir <path>] [--version <tag>]

  --dir      install location (default: ~/.local/bin)
  --version  release tag such as v0.2.0 (default: latest)
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --dir) INSTALL_DIR="$2"; shift 2 ;;
    --dir=*) INSTALL_DIR="${1#--dir=}"; shift ;;
    --version) VERSION="$2"; shift 2 ;;
    --version=*) VERSION="${1#--version=}"; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1" >&2; usage; exit 2 ;;
  esac
done

for tool in curl tar; do
  command -v "$tool" >/dev/null 2>&1 || { echo "error: $tool is required" >&2; exit 1; }
done

os="$(uname -s)"
case "$os" in
  Darwin) os="darwin" ;;
  Linux)  os="linux" ;;
  *) echo "error: unsupported OS: $os (use the Windows installer, or build from source)" >&2; exit 1 ;;
esac

arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *) echo "error: unsupported architecture: $arch" >&2; exit 1 ;;
esac

if [ "$VERSION" = "latest" ]; then
  VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1)"
  [ -n "$VERSION" ] || { echo "error: could not resolve the latest release tag" >&2; exit 1; }
fi

archive="cortex_${VERSION}_${os}_${arch}.tar.gz"
base="https://github.com/${REPO}/releases/download/${VERSION}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "downloading ${archive} (${VERSION})"
curl -fsSL "${base}/${archive}" -o "${tmp}/${archive}"
curl -fsSL "${base}/SHA256SUMS" -o "${tmp}/SHA256SUMS"

echo "verifying checksum"
if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "${tmp}/${archive}" | cut -d' ' -f1)"
else
  actual="$(shasum -a 256 "${tmp}/${archive}" | cut -d' ' -f1)"
fi
expected="$(grep "${archive}" "${tmp}/SHA256SUMS" | cut -d' ' -f1)"
[ -n "$expected" ] || { echo "error: ${archive} is absent from SHA256SUMS" >&2; exit 1; }
[ "$actual" = "$expected" ] || {
  echo "error: checksum mismatch for ${archive}" >&2
  echo "  expected ${expected}" >&2
  echo "  actual   ${actual}" >&2
  exit 1
}

tar -xzf "${tmp}/${archive}" -C "$tmp"
mkdir -p "$INSTALL_DIR"
extracted=""
for cand in "${tmp}/cortex_${VERSION}_${os}_${arch}/cortex" \
            "${tmp}/repocortex_${VERSION}_${os}_${arch}/repocortex" \
            "${tmp}/cortex_${VERSION}_${os}_${arch}/cortex" \
            "${tmp}/cortex" "${tmp}/cortex"; do
  if [ -f "$cand" ]; then
    extracted="$cand"
    break
  fi
done
[ -n "$extracted" ] || { echo "error: could not find binary in archive" >&2; exit 1; }

install -m 0755 "$extracted" "${INSTALL_DIR}/cortex" 2>/dev/null \
  || { cp "$extracted" "${INSTALL_DIR}/cortex" && chmod 0755 "${INSTALL_DIR}/cortex"; }
ln -sf "${INSTALL_DIR}/cortex" "${INSTALL_DIR}/repocortex" 2>/dev/null || cp "${INSTALL_DIR}/cortex" "${INSTALL_DIR}/repocortex"

# Gatekeeper quarantines anything curl wrote. Without this the first run dies
# with a dialog the user cannot dismiss from a terminal.
if [ "$os" = "darwin" ] && command -v xattr >/dev/null 2>&1; then
  xattr -d com.apple.quarantine "${INSTALL_DIR}/cortex" 2>/dev/null || true
  xattr -d com.apple.quarantine "${INSTALL_DIR}/repocortex" 2>/dev/null || true
fi

echo "installed ${INSTALL_DIR}/cortex (with repocortex alias)"
"${INSTALL_DIR}/cortex" version

case ":${PATH}:" in
  *":${INSTALL_DIR}:"*) ;;
  *) echo ""; echo "note: ${INSTALL_DIR} is not on PATH. Add:"; echo "  export PATH=\"${INSTALL_DIR}:\$PATH\"" ;;
esac

echo ""
echo "Next: cd into a repository and run"
echo "  cortex init && cortex index && cortex resolve"
echo "  cortex install     # register with your coding agent"
