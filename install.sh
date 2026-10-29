#!/bin/sh
# Installs tap from its GitHub release: the archive for this computer, checked against the
# release's checksums, into ~/.local/bin (or $TAP_INSTALL_DIR). Run it again to update.
#
#   curl -fsSL https://raw.githubusercontent.com/fschrhunt/tap/main/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/fschrhunt/tap/main/install.sh | sh -s -- --version v1.0.0
#
# $TAP_RELEASES replaces https://github.com/fschrhunt/tap/releases, for testing.
set -eu

releases=${TAP_RELEASES:-https://github.com/fschrhunt/tap/releases}
dir=${TAP_INSTALL_DIR:-$HOME/.local/bin}
version=${TAP_VERSION:-}

fail() { echo "tap install: $*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case $1 in
    --version) [ $# -ge 2 ] || fail "--version needs a version, like v1.0.0"; version=$2; shift 2 ;;
    --version=*) version=${1#--version=}; shift ;;
    --dir) [ $# -ge 2 ] || fail "--dir needs a folder"; dir=$2; shift 2 ;;
    -h|--help) sed -n '2,8p' "$0" 2>/dev/null | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) fail "unknown option $1; options: --version vX.Y.Z, --dir DIR" ;;
  esac
done

case $(uname -s) in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) fail "tap runs on macOS and Linux; this is $(uname -s)" ;;
esac
case $(uname -m) in
  arm64|aarch64) arch=arm64 ;;
  x86_64|amd64) arch=amd64 ;;
  *) fail "tap is built for arm64 and x86_64; this is $(uname -m)" ;;
esac

fetch() { # fetch URL FILE
  if command -v curl >/dev/null 2>&1; then curl -fsSL "$1" -o "$2"
  elif command -v wget >/dev/null 2>&1; then wget -q "$1" -O "$2"
  else fail "needs curl or wget"; fi
}

if [ -z "$version" ]; then
  # The latest release redirects to its tag; read the tag from where it lands.
  if command -v curl >/dev/null 2>&1; then
    landing=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$releases/latest") || fail "could not reach $releases"
  else
    landing=$(wget -q --max-redirect=5 --server-response --spider "$releases/latest" 2>&1 | sed -n 's/^ *[Ll]ocation: *//p' | tail -1 | tr -d '\r')
  fi
  version=${landing##*/}
fi
case $version in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  [0-9]*.[0-9]*.[0-9]*) version=v$version ;;
  *) fail "could not tell the latest version from $releases (got \"$version\")" ;;
esac

# A symlink in the install folder belongs to a package manager; do not replace it.
if [ -L "$dir/tap" ]; then
  fail "$dir/tap is a symlink; it likely belongs to a package manager, which should update it, or choose another folder with --dir"
fi

archive="tap_${version}_${os}_${arch}.tar.gz"
work=$(mktemp -d "${TMPDIR:-/tmp}/tap-install.XXXXXX")
trap 'rm -rf "$work"' EXIT INT TERM

echo "Installing tap $version for $os/$arch"
fetch "$releases/download/$version/$archive" "$work/$archive" || fail "could not download $archive"
fetch "$releases/download/$version/checksums.txt" "$work/checksums.txt" || fail "could not download checksums.txt"

expected=$(grep " $archive\$" "$work/checksums.txt" | cut -d' ' -f1)
if command -v sha256sum >/dev/null 2>&1; then actual=$(sha256sum "$work/$archive" | cut -d' ' -f1)
else actual=$(shasum -a 256 "$work/$archive" | cut -d' ' -f1); fi
if [ -z "$expected" ] || [ "$expected" != "$actual" ]; then
  fail "checksum mismatch for $archive; refusing to install"
fi

tar -xzf "$work/$archive" -C "$work" tap
if [ ! -f "$work/tap" ] || [ -L "$work/tap" ]; then
  fail "$archive has no tap binary"
fi
got=$("$work/tap" --version 2>/dev/null || true)
[ "$got" = "$version" ] || fail "$archive says it is \"$got\", not $version"

mkdir -p "$dir"
cp "$work/tap" "$dir/.tap.next"
chmod 755 "$dir/.tap.next"
mv -f "$dir/.tap.next" "$dir/tap"
echo "Installed $dir/tap"

case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "Add $dir to your PATH, for example in ~/.zshrc or ~/.bashrc:"
     echo "  export PATH=\"$dir:\$PATH\"" ;;
esac
echo "Next: tap import"
