#!/usr/bin/env sh
# Packs the extension for Chrome/Edge and Firefox into ../dist/.
# Firefox gets the same files with firefox.json merged into the manifest.
set -eu
cd "$(dirname "$0")"
out=../dist; mkdir -p "$out"
ver=$(python3 -c 'import json;print(json.load(open("manifest.json"))["version"])')
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
files="background.js common.js options.html options.js icons _locales"
for target in chrome firefox; do
  d="$tmp/$target"; mkdir -p "$d"
  cp -r $files "$d/"
  python3 - "$target" "$d/manifest.json" <<'PY'
import json, sys
target, dst = sys.argv[1], sys.argv[2]
m = json.load(open("manifest.json"))
if target == "firefox":
    m.update(json.load(open("firefox.json")))
json.dump(m, open(dst, "w"), ensure_ascii=False, indent=2)
PY
  (cd "$d" && python3 -m zipfile -c "$OLDPWD/$out/indirgen-extension-$target-$ver.zip" $files manifest.json)
  echo "built $out/indirgen-extension-$target-$ver.zip"
done
