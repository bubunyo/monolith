#!/usr/bin/env bash
# Install monolith from GitHub releases.
#
#   curl -fsSL https://raw.githubusercontent.com/bubunyo/monolith/master/install.sh | bash
#
# Environment:
#   MONOLITH_VERSION      release to install, e.g. v0.1.0 (default: latest)
#   MONOLITH_INSTALL_DIR  where to put the binary (default: /usr/local/bin;
#                         sudo is used if it isn't writable)
#   MONOLITH_BASE_URL     download base, for mirrors and testing
#                         (default: https://github.com/bubunyo/monolith/releases/download)

set -euo pipefail

REPO="bubunyo/monolith"
VERSION="${MONOLITH_VERSION:-}"
INSTALL_DIR="${MONOLITH_INSTALL_DIR:-/usr/local/bin}"
BASE_URL="${MONOLITH_BASE_URL:-https://github.com/${REPO}/releases/download}"

say() { printf '%s\n' "$*"; }
die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

# monolith uses Apple's Virtualization.framework on Apple Silicon.
[ "$(uname -s)" = "Darwin" ] || die "monolith runs on macOS only (this is $(uname -s))"
[ "$(uname -m)" = "arm64" ] || die "monolith needs Apple Silicon (this is $(uname -m))"
for cmd in curl shasum tar codesign; do
	command -v "$cmd" >/dev/null || die "$cmd is required"
done

if [ -z "$VERSION" ]; then
	VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" |
		sed -n 's/^[[:space:]]*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1) ||
		die "could not look up the latest release (none published yet?); set MONOLITH_VERSION"
	[ -n "$VERSION" ] || die "no release found for ${REPO}"
fi
case "$VERSION" in v*) ;; *) VERSION="v${VERSION}" ;; esac

name="monolith_${VERSION#v}_darwin_arm64"
archive="${name}.tar.gz"
url="${BASE_URL}/${VERSION}"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

say "Downloading monolith ${VERSION}..."
curl -fsSL -o "${tmp}/${archive}" "${url}/${archive}" || die "download failed: ${url}/${archive}"
curl -fsSL -o "${tmp}/checksums.txt" "${url}/checksums.txt" || die "download failed: ${url}/checksums.txt"

# Verify the archive against the release's checksums before unpacking.
(cd "$tmp" && grep " ${archive}\$" checksums.txt | shasum -a 256 -c - >/dev/null 2>&1) ||
	die "checksum mismatch for ${archive}"

tar -xzf "${tmp}/${archive}" -C "$tmp"
bin="${tmp}/${name}/monolith"
[ -f "$bin" ] || die "${archive} has no monolith binary"
# The binary must carry its code signature and virtualization entitlement.
codesign --verify "$bin" 2>/dev/null || die "the monolith binary's signature is invalid"
codesign -d --entitlements - --xml "$bin" 2>/dev/null | grep -q com.apple.security.virtualization ||
	die "the monolith binary lacks the virtualization entitlement"

# sudo only if the directory (or the nearest existing parent we'd create it
# in) isn't writable.
sudo=""
writable_dir="$INSTALL_DIR"
while [ ! -d "$writable_dir" ]; do
	writable_dir=$(dirname "$writable_dir")
done
if [ ! -w "$writable_dir" ]; then
	command -v sudo >/dev/null || die "${INSTALL_DIR} isn't writable; set MONOLITH_INSTALL_DIR"
	say "Installing to ${INSTALL_DIR} needs sudo."
	sudo="sudo"
fi
$sudo mkdir -p "$INSTALL_DIR"
$sudo install -m 0755 "$bin" "${INSTALL_DIR}/monolith"
# curl doesn't set the quarantine flag, but an earlier browser download
# might have; Gatekeeper would refuse an un-notarized binary with it.
$sudo xattr -d com.apple.quarantine "${INSTALL_DIR}/monolith" 2>/dev/null || true

say "Installed $("${INSTALL_DIR}/monolith" --version) to ${INSTALL_DIR}/monolith"
case ":${PATH}:" in
*":${INSTALL_DIR}:"*) ;;
*) say "Note: ${INSTALL_DIR} isn't on your PATH." ;;
esac
