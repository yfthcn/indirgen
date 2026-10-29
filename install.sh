#!/bin/sh
# İndirgen installer.
#
#   curl -fsSL https://github.com/yfthcn/indirgen/releases/latest/download/install.sh | sh
#
# Downloads the release binary for this machine, checks it against the
# release's SHA256SUMS and runs "indirgen install", which puts it in
# ~/.local/bin and sets up the user service and the menu entry. No sudo.
#
# Options are passed on to "indirgen install" (e.g. "| sh -s -- -no-linger").
# INDIRGEN_VERSION=v1.0.0 picks a release instead of the latest one.
# Remove with: indirgen uninstall   (add -purge to delete the settings too)
set -eu

repo="yfthcn/indirgen"
version="${INDIRGEN_VERSION:-latest}"
if [ "$version" = "latest" ]; then
  base="https://github.com/$repo/releases/latest/download"
else
  base="https://github.com/$repo/releases/download/$version"
fi
base="${INDIRGEN_DOWNLOAD_URL:-$base}"

say() { printf '  %s\n' "$*"; }
die() { printf '  indirgen kurulumu: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -ne 0 ] || die "root olarak çalıştırma; kendi kullanıcınla çalıştır (sudo olmadan)."

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) die "desteklenmeyen sistem: $(uname -s)" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) die "desteklenmeyen işlemci: $(uname -m)" ;;
esac
asset="indirgen-$os-$arch"
case "$asset" in
  indirgen-linux-amd64 | indirgen-linux-arm64 | indirgen-darwin-arm64) ;;
  *) die "bu sistem için hazır sürüm yok ($os/$arch): kaynaktan derle (README → From source)" ;;
esac

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL --retry 3 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q -O "$2" "$1"; }
else
  die "curl ya da wget gerekli"
fi
if command -v sha256sum >/dev/null 2>&1; then
  sha() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  die "sha256sum gerekli"
fi

# Not /tmp: it is mounted noexec on some systems, and the binary runs from here.
cache="${XDG_CACHE_HOME:-$HOME/.cache}"
mkdir -p "$cache"
tmp="$(mktemp -d "$cache/indirgen-install.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT INT TERM

say "İndiriliyor: $asset ($version)"
fetch "$base/$asset" "$tmp/indirgen" || die "$base/$asset indirilemedi"
fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS" || die "SHA256SUMS indirilemedi"

want="$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1 }' "$tmp/SHA256SUMS")"
[ -n "$want" ] || die "SHA256SUMS içinde $asset yok"
got="$(sha "$tmp/indirgen")"
[ "$want" = "$got" ] || die "SHA-256 uyuşmadı (beklenen $want, gelen $got)"
say "SHA-256 doğrulandı"

chmod +x "$tmp/indirgen"
"$tmp/indirgen" install "$@"
