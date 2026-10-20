#!/bin/sh
# Builds static binaries for Linux (amd64, arm64) and Windows (amd64) into dist/,
# their SHA256 checksums and the licenses of the libraries which are compiled into the binaries
set -e
VERSION=$(grep 'const version' main.go | cut -d'"' -f2)
rm -rf dist
mkdir -p dist
for target in linux/amd64 linux/arm64 windows/amd64; do
  os=${target%/*}
  arch=${target#*/}
  ext=""
  [ "$os" = "windows" ] && ext=".exe"
  out="dist/oitc-nvidia-dump-$VERSION-$os-$arch$ext"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w" -o "$out" .
  echo "$out"
done

# Licenses of all modules of all binaries
licenses=dist/THIRD_PARTY_LICENSES.txt
echo "oitc-nvidia-dump $VERSION contains the following third-party libraries." > "$licenses"
for binary in dist/oitc-nvidia-dump-*; do
  go version -m "$binary" | awk '$1 == "dep" { print $2 "@" $3 }'
done | sort -u | while read -r module; do
  dir=$(go mod download -json "$module" | sed -n 's/.*"Dir": "\(.*\)".*/\1/p')
  printf '\n\n================================================================================\n%s\n================================================================================\n' "$module" >> "$licenses"
  found=""
  for file in "$dir"/LICENSE* "$dir"/License* "$dir"/COPYING* "$dir"/NOTICE*; do
    [ -f "$file" ] || continue
    printf '\n' >> "$licenses"
    cat "$file" >> "$licenses"
    found=1
  done
  if [ -z "$found" ]; then
    echo "No license file found for $module" >&2
    exit 1
  fi
done
# The Go standard library is compiled into the binaries as well
printf '\n\n================================================================================\nGo standard library %s\n================================================================================\n\n' "$(go env GOVERSION)" >> "$licenses"
cat "$(go env GOROOT)/LICENSE" >> "$licenses"
echo "$licenses"

cd dist && sha256sum oitc-nvidia-dump-$VERSION-* THIRD_PARTY_LICENSES.txt > SHA256SUMS
