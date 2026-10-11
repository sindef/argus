#!/bin/sh
set -eu

mkdir -p /source-bundle/argus-go
awk '{print $1 "@" $2}' third_party/go-modules.txt > /tmp/source-modules
xargs go mod download -json < /tmp/source-modules > /tmp/source-downloads.json
jq -r 'select(.Zip) | [.Path, .Version, .Zip] | @tsv' /tmp/source-downloads.json > /tmp/source-archives
while IFS="$(printf '\t')" read -r module version zip; do
    dest="/source-bundle/argus-go/$module@$version"
    mkdir -p "$(dirname "$dest")"
    cp "$zip" "$dest.zip"
    cp "${zip%.zip}.mod" "$dest.mod"
done < /tmp/source-archives
