#!/bin/sh
# Writes the licence texts of the Go toolchain and of every module compiled
# into the given binaries, for shipping next to them (BSD/MIT/Apache all ask
# for the notice to travel with binary distributions).
#
#   scripts/third-party-notices.sh OUT BINARY...
#
# The module list is read from the binaries themselves (`go version -m`), so
# it is exactly what was linked: no test-only or unused dependency, and no
# tool to download. Licence files come from the module cache, which a build
# has just filled. A module whose licence can't be found fails the run:
# better a broken build than an image that ships code without its notice.
set -eu

[ $# -ge 2 ] || { echo "usage: $0 OUT BINARY..." >&2; exit 2; }
out=$1
shift

modcache=$(go env GOMODCACHE)
goroot=$(go env GOROOT)

# Module paths are stored case-encoded in the cache: "A" → "!a".
escape() { printf '%s' "$1" | sed 's/[A-Z]/!&/g' | tr 'A-Z' 'a-z'; }

licence_files() {
	find "$1" -maxdepth 1 -type f \( -iname 'LICENSE*' -o -iname 'LICENCE*' \
		-o -iname 'COPYING*' -o -iname 'NOTICE*' -o -iname 'UNLICENSE*' \) | sort
}

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

# "mod"/"dep <path> <version> [<sum>]" and "=> <path> <version>" (a replace)
# lines.
# A replace to a local directory keeps its path (licence read from there;
# run from the repository root).
for bin in "$@"; do
	go version -m "$bin" | awk '
		function flush() { if (pending) print path, ver; pending = 0 }
		# main module: a third-party tool (wireproxy), not this repo (devel)
		$1 == "mod" { flush(); if ($2 != "mycelium" && $3 != "(devel)") { path = $2; ver = $3; pending = 1 }; next }
		$1 == "dep" { flush(); path = $2; ver = $3; pending = 1; next }
		$1 == "=>" && pending {
			path = $2
			ver = ($2 ~ /^[.\/]/) ? "(local)" : $3
			flush(); next
		}
		{ flush() }
		END { flush() }
	'
done | sort -u > "$tmp"

{
	echo "Third-party notices"
	echo "==================="
	echo
	echo "This image contains software from the projects below, each under its"
	echo "own licence, reproduced here."
	echo
	echo "--------------------------------------------------------------------"
	echo "Go standard library and runtime ($(go env GOVERSION))"
	echo "--------------------------------------------------------------------"
	cat "$goroot/LICENSE"
	echo
	missing=0
	while read -r path ver; do
		case "$path" in
		./* | ../* | /*) dir=$path ;; # replace to a local directory (stipes-sdk)
		*) dir="$modcache/$(escape "$path")@$(escape "$ver")" ;;
		esac
		files=$( [ -d "$dir" ] && licence_files "$dir" || true )
		if [ -z "$files" ]; then
			echo "error: no licence file for $path $ver ($dir)" >&2
			missing=1
			continue
		fi
		echo "--------------------------------------------------------------------"
		echo "$path $ver"
		echo "--------------------------------------------------------------------"
		for f in $files; do
			cat "$f"
			echo
		done
	done < "$tmp"
	[ "$missing" -eq 0 ] || exit 1
} > "$out"

echo "wrote $out ($(wc -l < "$tmp") modules)" >&2
