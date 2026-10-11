#!/bin/sh
set -eu

cd "$(dirname "$0")/.."
mode=${1:---write}
case "$mode" in --write|--check) ;; *) echo 'usage: update-notices.sh [--write|--check]' >&2; exit 2 ;; esac
export CGO_ENABLED=0
export LC_ALL=C
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT HUP INT TERM
mkdir -p "$out/licenses"

collector() {
    go run github.com/google/go-licenses/v2@v2.0.1 "$@" --ignore argus ./cmd/argus ./cmd/argus-manager ./cmd/argus-hostd
}

collector check --allowed_licenses=Apache-2.0,BSD-2-Clause,BSD-3-Clause,ISC,MIT
printf '%s\n' '{{range .}}{{.Name}},{{.LicensePath}},{{.LicenseName}}{{"\n"}}{{end}}' > "$out/template"
collector report --template="$out/template" > "$out/report"
awk -F ',' 'BEGIN {OFS = ","} NF == 3 && $1 != "" {n = split($2, parts, "/"); $2 = "third_party/licenses/go/" $1 "/" parts[n]; print}' "$out/report" | sort > "$out/go-licenses.csv"
collector save --save_path="$out/licenses/go"
while IFS=',' read -r package path license; do
    test -s "$out/${path#third_party/}"
done < "$out/go-licenses.csv"
go list -deps -f '{{if .Module}}{{.Module.Path}} {{.Module.Version}}{{end}}' ./cmd/argus ./cmd/argus-manager ./cmd/argus-hostd > "$out/modules"
awk 'NF == 2 && $1 != "argus"' "$out/modules" | sort -u > "$out/go-modules.txt"
go version | awk '{print $3}' > "$out/go-toolchain.txt"
goroot=$(go env GOROOT)
mkdir -p "$out/licenses/go-runtime"
cp "$goroot/LICENSE" "$goroot/PATENTS" "$out/licenses/go-runtime/"
find "$goroot/src/vendor" -type f \( -name LICENSE -o -name PATENTS -o -name NOTICE \) |
    while IFS= read -r file; do
        rel=${file#"$goroot/"}
        mkdir -p "$out/licenses/go-runtime/$(dirname "$rel")"
        cp "$file" "$out/licenses/go-runtime/$rel"
    done

for dir in web/static/vendor/*; do
    test -d "$dir" || continue
    test -n "$(find "$dir" -type f -name 'LICENSE*' -print -quit)" || { echo "Missing license: $dir" >&2; exit 1; }
done
find web/static/vendor -type f \( -name 'LICENSE*' -o -name 'NOTICE*' -o -name 'COPYING*' -o -name AUTHORS \) |
    while IFS= read -r file; do
        rel=${file#web/static/vendor/}
        mkdir -p "$out/licenses/web/$(dirname "$rel")"
        cp "$file" "$out/licenses/web/$rel"
    done
find web/static/vendor -type f -print | sort | while IFS= read -r file; do sha256sum "$file"; done > "$out/web-assets.sha256"

for name in go-licenses.csv go-modules.txt go-toolchain.txt web-assets.sha256 licenses/go licenses/go-runtime licenses/web; do
    if [ "$mode" = --check ]; then
        diff -ru "third_party/$name" "$out/$name"
    else
        mkdir -p "third_party/$(dirname "$name")"
        rm -rf "third_party/$name"
        cp -R "$out/$name" "third_party/$name"
    fi
done
