#!/bin/sh
# Install the latest billkit CLI release for your OS/arch.
#
#   curl -fsSL https://raw.githubusercontent.com/billkit-eu/billkit-cli/main/install.sh | sh
#
# Override the install dir with BILLKIT_INSTALL_DIR (default: /usr/local/bin),
# and the release with BILLKIT_VERSION (default: the latest one).
set -eu

# The public mirror. Development happens in the private BillKit monorepo; the
# releases, the tags and this script's own canonical copy live here, so an
# anonymous curl can reach every URL below.
REPO="billkit-eu/billkit-cli"
INSTALL_DIR="${BILLKIT_INSTALL_DIR:-/usr/local/bin}"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
  x86_64 | amd64) arch="amd64" ;;
  arm64 | aarch64) arch="arm64" ;;
  *) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac
case "$os" in
  linux | darwin) ;;
  *) echo "unsupported OS: $os — on Windows use the .zip from the Releases page" >&2; exit 1 ;;
esac

# Resolve the release to install. Tags here are plain vX.Y.Z, and every release
# in this repo is a CLI release, so GitHub's own "latest" is the right answer
# and there is no page of unrelated releases to grep past.
if [ -n "${BILLKIT_VERSION:-}" ]; then
  tag="v${BILLKIT_VERSION#v}"
else
  tag="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep -o '"tag_name": *"[^"]*"' \
    | head -n 1 | sed 's/.*"tag_name": *"//; s/"$//')"
fi
if [ -z "${tag:-}" ]; then
  echo "could not determine the latest billkit CLI release" >&2
  exit 1
fi
version="${tag#v}"

archive="billkit_${version}_${os}_${arch}.tar.gz"
base="https://github.com/${REPO}/releases/download/${tag}"

echo "Downloading ${archive}…"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "${base}/${archive}" -o "$tmp/$archive"

# ── Verify before it becomes an executable on PATH ────────────────────────
#
# GoReleaser publishes checksums.txt beside the archives (see
# .goreleaser.yaml's `checksum:` block) and this script used to fetch neither
# it nor anything else, so a truncated download or a swapped asset became a
# binary in /usr/local/bin that holds bk_live_ keys.
#
# Be honest about what this buys: checksums.txt comes down the same channel
# as the archive, so it proves integrity — a partial download, a corrupted
# mirror, a mangled proxy — and not provenance. Anyone who can rewrite the
# release assets can rewrite both files. Closing *that* needs a signature
# over the checksums (a `signs:` block and a published key), which is worth
# doing and is not what this line is.
curl -fsSL "${base}/checksums.txt" -o "$tmp/checksums.txt"

if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$tmp/$archive" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  actual="$(shasum -a 256 "$tmp/$archive" | awk '{print $1}')"
else
  echo "no sha256 tool found (looked for sha256sum and shasum)." >&2
  echo "Refusing to install an unverified binary. Install coreutils, or" >&2
  echo "download and check ${base}/checksums.txt by hand." >&2
  exit 1
fi

# Match the one line that names our archive, by exact string rather than by
# pattern: the filename is full of dots, and `sha256sum -c` over the whole
# file would try to verify every other platform's archive and fail on the
# ones we deliberately did not download. The `*` form is what a checksum
# tool writes for binary mode; accept either spelling.
expected="$(awk -v f="$archive" '$2 == f || $2 == "*" f { print $1 }' "$tmp/checksums.txt")"
if [ -z "$expected" ]; then
  echo "checksums.txt for ${tag} lists no entry for ${archive}." >&2
  exit 1
fi
if [ "$actual" != "$expected" ]; then
  echo "checksum mismatch for ${archive} — refusing to install." >&2
  echo "  expected ${expected}" >&2
  echo "  actual   ${actual}" >&2
  exit 1
fi

tar -xzf "$tmp/$archive" -C "$tmp"

# `install`, not `mv`, and that is the whole point of this branch.
#
# `sudo mv` moved a file created by the *invoking* user into a root-owned
# directory, and a move keeps the source's ownership and mode. The result was
# a binary at /usr/local/bin/billkit owned by an unprivileged account, which
# that account could then rewrite at will — a root-path executable anyone
# local could replace, for a tool that stores live API keys. The `chmod +x`
# that followed had the same bug from the other side: no sudo, so on the
# root-owned path it was operating on a file it might not own.
#
# `install -m 0755` copies instead of moving, so the destination is created
# by the effective user (root under sudo) and the mode is set in the same
# call. Present on macOS and on every Linux distribution.
if [ -w "$INSTALL_DIR" ]; then
  install -m 0755 "$tmp/billkit" "$INSTALL_DIR/billkit"
else
  echo "Installing to $INSTALL_DIR (sudo)…"
  sudo install -m 0755 "$tmp/billkit" "$INSTALL_DIR/billkit"
fi

echo "✓ Installed billkit ${version} to ${INSTALL_DIR}/billkit"
