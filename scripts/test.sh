#!/bin/sh
set -eu

coverage_dir=$(mktemp -d)
trap 'rm -rf "$coverage_dir"' EXIT
INFLOW_EXAMPLE_COVERAGE_DIR="$coverage_dir" go test -race -coverprofile="$coverage_dir/unit.out" ./...
go tool covdata textfmt -i="$coverage_dir" -o="$coverage_dir/commands.out"
# Combine unit coverage with coverage from the actual example executables.
awk '
FNR == 1 { next }
{ statements[$1] = $2; hits[$1] += $3 }
END {
    print "mode: atomic"
    for (block in statements) print block, statements[block], hits[block]
}' "$coverage_dir/unit.out" "$coverage_dir/commands.out" > coverage.out
awk -f scripts/check-coverage.awk coverage.out
