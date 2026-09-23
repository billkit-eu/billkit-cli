#!/bin/sh
# Install the latest billkit CLI release for your OS/arch.
#
#   curl -fsSL https://raw.githubusercontent.com/billkit-eu/billkit-cli/main/install.sh | sh
#
# Override the install dir with BILLKIT_INSTALL_DIR (default: /usr/local/bin),
# and the release with BILLKIT_VERSION (default: the latest one).
set -eu

# The public mirror. Development happens in the private BillKit monorepo; the
# releases and the tags live here, so an anonymous curl can reach every URL
# below. The `curl | sh` line in the README fetches this file from the
# mirror's own `main`, where `mise run split` puts a copy of it, so edit it
# here, in the monorepo, which is the canonical copy, and never in the mirror.
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
#
# Two ways to ask, and the order matters. The web endpoint redirects
# /releases/latest to /releases/tag/vX.Y.Z and is not rate limited, so a HEAD
# request for the effective URL gives the tag as the last path segment.
# api.github.com gives 60 requests per hour per IP to anonymous callers, which
# a CI runner or anyone behind a shared NAT burns through without knowing it,
# and then this script reported "could not determine the latest billkit CLI
# release", which reads as "the release is missing" rather than "you were
# throttled". So the redirect is asked first and the API is the fallback.
if [ -n "${BILLKIT_VERSION:-}" ]; then
  tag="v${BILLKIT_VERSION#v}"
else
  resolved="$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
    "https://github.com/${REPO}/releases/latest" 2>/dev/null || true)"
  case "$resolved" in
    */releases/tag/*) tag="${resolved##*/}" ;;
    *) tag="" ;;
  esac
  if [ -z "$tag" ]; then
    tag="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
      | grep -o '"tag_name": *"[^"]*"' \
      | head -n 1 | sed 's/.*"tag_name": *"//; s/"$//')"
  fi
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
# Two separate questions, in the order they have to be asked.
#
# 1. Provenance: did these bytes come from us? checksums.txt travels down the
#    same channel as the archive, so on its own it proves integrity and
#    nothing else — anyone who can rewrite one asset can rewrite both. The
#    release workflow therefore signs checksums.txt with cosign, keyless, and
#    publishes the signature and certificate beside it. Verifying that first
#    is what makes trusting the checksums meaningful at all.
#
# 2. Integrity: are these the bytes checksums.txt names? That is the sha256
#    comparison further down, and it catches the ordinary failures — a
#    truncated download, a corrupted mirror, a mangling proxy.
#
# cosign is not a dependency of this script. Requiring it would break
# `curl | sh` for almost everybody, and the checksum alone is still worth far
# more than the nothing this script used to do. So: verify when cosign is
# here, say plainly when it is not, and refuse only on an actual mismatch.
curl -fsSL "${base}/checksums.txt" -o "$tmp/checksums.txt"

# The identity a keyless signature carries is the workflow that produced it,
# at the ref it ran on. Pinning the exact tag matters: without it, a signature
# from any tag of this repo would satisfy any other, and a downgrade to an
# older release would verify cleanly.
cert_identity="https://github.com/${REPO}/.github/workflows/publish.yml@refs/tags/${tag}"
oidc_issuer="https://token.actions.githubusercontent.com"

if command -v cosign >/dev/null 2>&1; then
  if curl -fsSL "${base}/checksums.txt.sig" -o "$tmp/checksums.txt.sig" 2>/dev/null &&
     curl -fsSL "${base}/checksums.txt.pem" -o "$tmp/checksums.txt.pem" 2>/dev/null; then
    if cosign verify-blob \
        --signature "$tmp/checksums.txt.sig" \
        --certificate "$tmp/checksums.txt.pem" \
        --certificate-identity "$cert_identity" \
        --certificate-oidc-issuer "$oidc_issuer" \
        "$tmp/checksums.txt" >/dev/null 2>&1; then
      echo "✓ Provenance verified: ${tag} was built and signed by ${REPO}'s release workflow."
    else
      echo "the signature on checksums.txt for ${tag} did not verify." >&2
      echo "Refusing to install. This is not a network problem: the checksums do not" >&2
      echo "carry a valid signature from ${cert_identity}." >&2
      exit 1
    fi
  else
    # Every release from the version that added the `signs:` block onward has
    # one. An older release legitimately does not, and refusing to install it
    # would be wrong.
    echo "note: ${tag} publishes no checksum signature, so provenance was not checked." >&2
  fi
else
  echo "note: cosign is not installed, so the download was checked for integrity" >&2
  echo "      but not for provenance. To check who built it:" >&2
  echo "        cosign verify-blob --signature checksums.txt.sig \\" >&2
  echo "          --certificate checksums.txt.pem \\" >&2
  echo "          --certificate-identity '${cert_identity}' \\" >&2
  echo "          --certificate-oidc-issuer '${oidc_issuer}' checksums.txt" >&2
fi

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
