#!/bin/sh
set -eu
export LC_ALL=C
export GOTOOLCHAIN=local

test "$(awk -F '\t' '$1 == "k3s" {print $2}' /infrastructure-sources.tsv)" = "$K3S_VERSION"
test "$(awk -F '\t' '$1 == "kairos" {print $2}' /infrastructure-sources.tsv)" = "$KAIROS_INIT_VERSION"
mkdir -p /source-bundle/upstream /source-bundle/go /notices/licenses /upstream
cp /infrastructure-sources.tsv /notices/

copy_notices() {
    root=$1
    dest=$2
    find "$root" -type f \( -iname 'LICEN[CS]E*' -o -iname 'NOTICE*' -o -iname 'COPYING*' -o -iname 'COPYRIGHT*' -o -name PATENTS \) > /notice-files
    if [ ! -s /notice-files ]; then
        find "$root" -maxdepth 1 -type f -iname 'README*' -exec grep -El 'Permission is hereby granted|Redistribution and use|Apache License|General Public License|Mozilla Public License' {} \; > /notice-files
    fi
    if [ ! -s /notice-files ] && [ -n "${3:-}" ] && [ -d "/infrastructure-licenses/$3" ]; then
        root="/infrastructure-licenses/$3"
        find "$root" -type f > /notice-files
    fi
    if [ ! -s /notice-files ]; then
        printf '%s\n' "$root" >> /unresolved-licenses
        return
    fi
    while IFS= read -r file; do
        rel=${file#"$root/"}
        mkdir -p "$dest/$(dirname "$rel")"
        cp "$file" "$dest/$rel"
    done < /notice-files
}

while IFS="$(printf '\t')" read -r name version checksum url; do
    archive="/source-bundle/upstream/$name.tar.gz"
    curl --fail --location --silent --show-error --retry 3 "$url" -o "$archive"
    printf '%s  %s\n' "$checksum" "$archive" | sha256sum --check
    mkdir -p "/upstream/$name"
    tar -xzf "$archive" --strip-components=1 -C "/upstream/$name"
    copy_notices "/upstream/$name" "/notices/licenses/$name"
    test -f "/upstream/$name/go.mod" || continue
    cd "/upstream/$name"
    case "$name" in
        k3s|kairos|containerd) targets='./cmd/...' ;;
        plugins) targets='./plugins/...' ;;
        cosign) targets='./cmd/cosign' ;;
        *) targets='./...' ;;
    esac
    go list -deps -f '{{if .Module}}{{if .Module.Replace}}{{.Module.Replace.Path}} {{.Module.Replace.Version}}{{else}}{{.Module.Path}} {{.Module.Version}}{{end}}{{end}}' "$targets" > /linked-modules
    awk 'NF == 2 {print $1 "@" $2}' /linked-modules | sort -u > "/notices/$name-go-modules.txt"
    xargs go mod download -json < "/notices/$name-go-modules.txt" > "/notices/$name-go-downloads.json"
    jq -r 'select(.Dir and .Zip) | [.Path, .Version, .Dir, .Zip] | @tsv' "/notices/$name-go-downloads.json" > /modules
    while IFS="$(printf '\t')" read -r module tag dir zip; do
        dest="/source-bundle/go/$module@$tag"
        mkdir -p "$(dirname "$dest")"
        cp "$zip" "$dest.zip"
        cp "${zip%.zip}.mod" "$dest.mod"
        copy_notices "$dir" "/notices/licenses/go/$module@$tag" "$module@$tag"
    done < /modules
done < /infrastructure-sources.tsv
if [ -s /unresolved-licenses ]; then
    echo 'Missing upstream licenses:' >&2
    cat /unresolved-licenses >&2
    exit 1
fi
cp -R /notices /source-bundle/infrastructure-notices
