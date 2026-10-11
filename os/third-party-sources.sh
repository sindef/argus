#!/bin/sh
set -eu

dest=/source-bundle
mkdir -p "$dest/ubuntu"
cp /usr/share/doc/argus/third_party/os-packages.tsv "$dest/"
cp -R /usr/share/doc/argus "$dest/notices"
sed -i 's/^Types: deb$/Types: deb deb-src/' /etc/apt/sources.list.d/ubuntu.sources
apt-get update
cut -f3,4 "$dest/os-packages.tsv" | LC_ALL=C sort -u > "$dest/source-packages.tsv"
cd "$dest/ubuntu"
set --
while IFS="$(printf '\t')" read -r source version; do
    set -- "$@" "$source=$version"
done < "$dest/source-packages.tsv"
test "$#" -gt 0
apt-get source --download-only --only-source "$@"
cd "$dest"
find . -type f ! -name SHA256SUMS -print0 | LC_ALL=C sort -z | xargs -0 sha256sum > SHA256SUMS
