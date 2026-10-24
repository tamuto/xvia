#!/bin/sh
# Build release binaries and checksums into dist/.
#
#   scripts/build-release.sh v0.1.0
#
# The version must be an existing tag pointing at HEAD, and the working tree
# must be clean, so that the published binaries match the tagged source.
set -eu

version=${1:?usage: scripts/build-release.sh vX.Y.Z}
case "$version" in
v[0-9]*.[0-9]*.[0-9]*) ;;
*) echo "version must look like vX.Y.Z" >&2; exit 1 ;;
esac

cd "$(dirname "$0")/.."
[ -z "$(git status --porcelain --untracked-files=no)" ] || { echo "working tree is not clean" >&2; exit 1; }
[ "$(git rev-parse "$version^{commit}" 2>/dev/null)" = "$(git rev-parse HEAD)" ] ||
	{ echo "tag $version does not exist or does not point at HEAD" >&2; exit 1; }

rm -rf dist
mkdir dist
for arch in amd64 arm64; do
	echo "building linux/$arch"
	CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
		-ldflags="-s -w -X main.version=$version" \
		-o "dist/xvia-linux-$arch" ./cmd/xvia
done
(cd dist && sha256sum xvia-linux-* > SHA256SUMS)
echo
cat dist/SHA256SUMS
echo
echo "dist/ is ready; upload its files to the GitHub release $version"
