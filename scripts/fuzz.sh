#!/bin/sh
# Runs every Fuzz* target for FUZZTIME (default 10s). New failing inputs land
# in the package's testdata/fuzz and fail the run.
set -eu
cd "$(dirname "$0")/.."
time=${FUZZTIME:-10s}
grep -rl --include='*_test.go' '^func Fuzz' internal | sort | while read -r file; do
	pkg=./$(dirname "$file")
	sed -n 's/^func \(Fuzz[A-Za-z0-9_]*\)(.*/\1/p' "$file" | while read -r target; do
		echo "fuzz $pkg $target"
		go test -run '^$' -fuzz "^${target}\$" -fuzztime "$time" "$pkg" </dev/null
	done
done
