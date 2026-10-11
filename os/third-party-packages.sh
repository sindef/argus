#!/bin/sh
set -eu

dest=/usr/share/doc/argus/third_party
mkdir -p "$dest/licenses/os"
inventory=$(mktemp)
trap 'rm -f "$inventory"' EXIT HUP INT TERM
dpkg-query -W -f='${db:Status-Abbrev}\t${binary:Package}\t${Version}\t${source:Package}\t${source:Version}\n' > "$inventory"
awk -F '\t' '$1 ~ /^.i/ {print $2 "\t" $3 "\t" $4 "\t" $5}' "$inventory" |
    LC_ALL=C sort > "$dest/os-packages.tsv"
test -s "$dest/os-packages.tsv"

while IFS="$(printf '\t')" read -r package version source source_version; do
    name=${package%%:*}
    copyright="/usr/share/doc/$name/copyright"
    test -r "$copyright" || { echo "Missing package copyright: $package" >&2; exit 1; }
    cp -L "$copyright" "$dest/licenses/os/$name.copyright"
done < "$dest/os-packages.tsv"
test -d /usr/share/common-licenses
