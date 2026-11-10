#!/bin/sh
# Runs the comparison tests in their container (see README.md).
#
#   run.sh image             build the desktop image for DESKTOP_VERSION
#   run.sh live [args]       desktop client against grokbot-tunnel
#   run.sh record [args]     the same, and write the desktop's answers to testdata/compat/<version>
#   run.sh golden [args]     grokbot-tunnel against the recorded answers
#   run.sh oracle [args]     single Go functions against the desktop's, on random inputs
#
# args go to the test binary, e.g. -test.run TestLive/shell. Reports and the
# transcripts of the run are written to COMPARE_OUT (tests/compare/out/<version>).
# The desktop's AppImage is looked for in DESKTOP_DIR (tests/compare/desktop).
# Needs docker; set DOCKER="sudo docker" if your user may not use it directly.
set -eu
docker=${DOCKER:-docker}
cd "$(dirname "$0")/../.."
mode=${1:?usage: run.sh image|live|record|golden|oracle [test arguments]}
shift
current=$(sed -n 's/^const Version = "\(.*\)"$/\1/p' internal/pkg/compattest/compattest.go)
version=${DESKTOP_VERSION:-$current}
lock=tests/compare/docker/desktop.lock

build_base() {
	$docker build -q --target base -t grokbot-compare:base tests/compare/docker >/dev/null
}

# Builds grokbot-compare:<version> with the bundle of that desktop release.
build_desktop() {
	entry=$(grep "^$version " "$lock") || { echo "run.sh: $lock has no entry for $version" >&2; exit 1; }
	sha=$(echo "$entry" | cut -d' ' -f2)
	url=$(echo "$entry" | cut -d' ' -f3)
	dir=${DESKTOP_DIR:-tests/compare/desktop}
	appimage=$dir/Grok_Bot_$version.AppImage
	if [ ! -f "$appimage" ]; then
		[ "$url" != - ] || { echo "run.sh: $appimage is missing and $lock has no URL for it" >&2; exit 1; }
		mkdir -p "$dir"
		curl -fL --retry 3 -o "$appimage.part" "$url"
		mv "$appimage.part" "$appimage"
	fi
	# A build context with nothing but the AppImage.
	context=$(mktemp -d)
	ln "$appimage" "$context/" 2>/dev/null || cp "$appimage" "$context/"
	$docker build -q --target desktop \
		--build-arg "DESKTOP_VERSION=$version" --build-arg "DESKTOP_SHA256=$sha" \
		--build-context "appimage=$context" -t "grokbot-compare:$version" tests/compare/docker >/dev/null
	rm -rf "$context"
}

case $mode in
image)
	build_desktop
	echo "built grokbot-compare:$version"
	exit
	;;
live | record | oracle)
	image=grokbot-compare:$version
	$docker image inspect "$image" >/dev/null 2>&1 || build_desktop
	run=TestLive
	;;
golden)
	image=grokbot-compare:base
	build_base
	run=TestGolden
	;;
*)
	echo "run.sh: unknown mode $mode" >&2
	exit 1
	;;
esac

out=${COMPARE_OUT:-tests/compare/out/$version}
stage=$(mktemp -d)
cid=
# shellcheck disable=SC2317,SC2329 # called by the trap
cleanup() {
	[ -z "$cid" ] || $docker rm -f "$cid" >/dev/null
	rm -rf "$stage"
}
trap cleanup EXIT
mkdir -p "$stage/bin"
if [ "$mode" = oracle ]; then
	# A test binary for every package with oracle tests.
	packages=$(find internal -name 'oracle*_test.go' -exec dirname {} \; | sort -u | sed 's|^|./|')
	# shellcheck disable=SC2086 # a list of packages
	CGO_ENABLED=0 go test -c -tags compare -o "$stage/bin/oracle/" $packages
else
	CGO_ENABLED=0 go build -o "$stage/bin/grokbot-tunnel" ./cmd/grokbot-tunnel
	CGO_ENABLED=0 go test -c -tags compare -o "$stage/bin/compare.test" ./tests/compare
fi
cp -r tests/compare/js "$stage/js"
cp -r testdata/compat "$stage/testdata"
chmod -R a+rX "$stage"

# No network: the clients talk to the fake backend on the container's
# loopback and to nothing else. The host name is the computer's label in the
# hello, so it is the same in every run.
cid=$($docker run -d --network none --hostname compare -v "$stage:/compare:ro" -e COMPARE_JS=/compare/js "$image" sleep infinity)

if [ "$mode" = oracle ]; then
	status=0
	for binary in "$stage"/bin/oracle/*.test; do
		$docker exec "$cid" "/compare/bin/oracle/$(basename "$binary")" -test.run Oracle "$@" || status=$?
	done
	exit $status
fi

full=true
[ $# -eq 0 ] || full=false
set -- -test.run "$run" "$@"
[ "$mode" != record ] || set -- "$@" -compare.record
# COMPARE_KEEP=1 also copies what each client left in its root to
# $COMPARE_OUT/raw, for looking into a difference.
keep=
[ -z "${COMPARE_KEEP:-}" ] || keep=/work/out/raw
status=0
$docker exec -e "COMPARE_KEEP=$keep" "$cid" /compare/bin/compare.test -test.v -test.timeout 120m "$@" || status=$?

# The results leave the container as a tar stream, so that they belong to
# the user who ran this script.
rm -rf "$out"
mkdir -p "$out"
$docker exec "$cid" sh -c 'mkdir -p /work/out && tar -C /work/out -cf - .' | tar -C "$out" -xf -
if [ "$mode" = record ]; then
	data=testdata/compat/$version
	mkdir -p "$data"
	# The reference outputs of the desktop's pure functions.
	# shellcheck disable=SC2016 # the container's variable
	$docker exec "$cid" sh -c 'node /compare/js/pure/reference.mjs "$GROKBOT_COMPARE_DESKTOP" /work/symbols.json' >"$data/pure-functions.json"
	$docker exec "$cid" cat /work/symbols.json >"$data/symbols.json"
	if [ -d "$out/transcripts" ]; then
		# A run of every scenario replaces the recordings, so that those of
		# scenarios that are gone go too; a run of some adds to them.
		! $full || rm -rf "$data/transcripts"
		mkdir -p "$data/transcripts"
		cp -r "$out/transcripts/." "$data/transcripts/"
		echo "recorded $(find "$out/transcripts" -name '*.json' | wc -l) transcripts in $data/transcripts"
	fi
fi
echo "report: $out/report.md"
exit $status
