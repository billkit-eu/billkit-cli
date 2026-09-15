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
url="https://github.com/${REPO}/releases/download/${tag}/${archive}"

echo "Downloading ${archive}…"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "$url" -o "$tmp/$archive"
tar -xzf "$tmp/$archive" -C "$tmp"

if [ -w "$INSTALL_DIR" ]; then
  mv "$tmp/billkit" "$INSTALL_DIR/billkit"
else
  echo "Installing to $INSTALL_DIR (sudo)…"
  sudo mv "$tmp/billkit" "$INSTALL_DIR/billkit"
fi
chmod +x "$INSTALL_DIR/billkit"

echo "✓ Installed billkit ${version} to ${INSTALL_DIR}/billkit"
