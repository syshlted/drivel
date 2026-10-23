#!/usr/bin/env bash
# dist.sh — the release artefacts, into $DIST: a tarball per platform, a .deb
# and an .rpm per Linux platform, and SHA256SUMS over all of them.
#
# Run it through `make dist`, which passes everything below as environment so
# the platform list, the output directory and the pinned nfpm are defined once,
# in the Makefile. The release workflow runs the same target, so an artefact
# built here and one built by CI come from one recipe.
#
#   DIST         output directory; emptied first
#   PLATFORMS    GOOS/GOARCH pairs, space-separated
#   NFPM         path to the nfpm binary
#   RELEASE_TAG  optional; when set, every binary must report exactly this
#                version, or nothing is produced
#
# The version is never passed in. Go records it in each binary when it builds
# one — the tag for a tagged commit, a pseudo-version otherwise, "+dirty" for a
# modified tree — and this script reads it back out of every binary it built
# with `go version -m`, which works for a binary this machine cannot run. That
# is what makes RELEASE_TAG a check rather than a label: a release built from
# the wrong commit, or from a tree with uncommitted changes, fails here.
set -euo pipefail

: "${DIST:?}" "${PLATFORMS:?}" "${NFPM:?}"
RELEASE_TAG=${RELEASE_TAG:-}

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
rm -rf "$DIST"
mkdir -p "$DIST"

# The tarballs carry the commit's time rather than now, so building the same
# commit twice gives the same bytes.
SOURCE_DATE_EPOCH=$(git log -1 --format=%ct)
export SOURCE_DATE_EPOCH

# -trimpath keeps this machine's paths out of the binary. -buildvcs=true makes
# a build without git information an error rather than a binary that says
# "(devel)": a release that cannot say what it is has no business existing.
build() { # GOOS GOARCH OUT
	CGO_ENABLED=0 GOOS=$1 GOARCH=$2 go build -trimpath -buildvcs=true -o "$3" ./cmd/drivel
}
stamped() { # BINARY -> the module version Go recorded in it
	go version -m "$1" | awk '$1 == "mod" { print $3; exit }'
}

# The completions and the check on `drivel version` need a binary this machine
# can run. Its flags are the cross-built ones', since no flag is defined in a
# platform-specific file, so its completion scripts serve every platform.
native="$stage/native/drivel"
build "$(go env GOOS)" "$(go env GOARCH)" "$native"
version=$(stamped "$native")
if [ -z "$version" ]; then
	echo "dist: no version recorded in the build" >&2
	exit 1
fi
if [ -n "$RELEASE_TAG" ] && [ "$version" != "$RELEASE_TAG" ]; then
	echo "dist: this checkout builds $version, not $RELEASE_TAG" >&2
	echo "dist: a release is built from a clean checkout of exactly that tag" >&2
	exit 1
fi
said=$("$native" version | head -1)
if [ "$said" != "drivel $version" ]; then
	echo "dist: 'drivel version' says '$said', want 'drivel $version'" >&2
	exit 1
fi
case $version in
*+dirty) echo "dist: warning: $version has uncommitted changes; fine for a trial, never for a release" >&2 ;;
esac

common="$stage/common"
mkdir -p "$common"
"$native" completion bash >"$common/drivel.bash"
"$native" completion zsh >"$common/_drivel"
gzip -9n <docs/user/drivel.1 >"$common/drivel.1.gz"

for platform in $PLATFORMS; do
	goos=${platform%/*}
	goarch=${platform#*/}
	dir="$stage/$goos-$goarch"
	mkdir -p "$dir"
	build "$goos" "$goarch" "$dir/drivel"
	got=$(stamped "$dir/drivel")
	if [ "$got" != "$version" ]; then
		echo "dist: $platform built $got, the native build $version" >&2
		exit 1
	fi

	name="drivel-$version-$goos-$goarch"
	root="$stage/tar/$name"
	mkdir -p "$root/completions"
	cp "$dir/drivel" "$root/"
	cp docs/user/drivel.1 LICENSE README.md CHANGELOG.md "$root/"
	cp "$common/drivel.bash" "$common/_drivel" "$root/completions/"
	tar --sort=name --owner=0 --group=0 --numeric-owner \
		--mtime="@$SOURCE_DATE_EPOCH" -C "$stage/tar" -cf - "$name" |
		gzip -9n >"$DIST/$name.tar.gz"
	echo "dist: $name.tar.gz"

	# nfpm makes the two Linux package formats and nothing else this script
	# needs; FreeBSD's pkg format is not one of them, so FreeBSD gets the tarball.
	if [ "$goos" = linux ]; then
		cp "$common/drivel.bash" "$common/_drivel" "$common/drivel.1.gz" LICENSE "$dir/"
		for format in deb rpm; do
			(cd "$dir" && DRIVEL_VERSION=$version DRIVEL_ARCH=$goarch \
				"$NFPM" package --config "$OLDPWD/packaging/nfpm.yaml" --packager "$format" --target "$DIST/") |
				sed 's/^/dist: /'
		done
	fi
done

# nullglob: a platform list with no Linux in it makes no packages, and an
# unmatched glob would otherwise reach sha256sum as a literal "*.deb".
shopt -s nullglob
(cd "$DIST" && sha256sum -- *.tar.gz *.deb *.rpm >SHA256SUMS)
echo "dist: SHA256SUMS"
