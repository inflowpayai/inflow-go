#!/bin/sh
set -eu

repository=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
consumer=$(mktemp -d)
trap 'rm -f "$consumer/main.go" "$consumer/go.mod" "$consumer/go.sum"; rmdir "$consumer"' EXIT

cp "$repository/testdata/consumer/main.go" "$consumer/main.go"
cd "$consumer"
export GOWORK=off
go mod init example.com/inflow-consumer
if [ "$#" -gt 1 ]; then
    echo "Usage: verify-consumer.sh [published-version]" >&2
    exit 1
fi
if [ "$#" -eq 0 ]; then
    go mod edit -require github.com/inflowpayai/inflow-go@v0.0.0
    go mod edit -replace "github.com/inflowpayai/inflow-go=$repository"
else
    export GOPROXY=https://proxy.golang.org
    export GOPRIVATE= GONOPROXY= GONOSUMDB=
    export GOSUMDB=sum.golang.org
    go mod edit -require "github.com/inflowpayai/inflow-go@$1"
fi
go mod tidy
if [ "$#" -eq 1 ]; then
    test "$(go list -m -f '{{.Version}}' github.com/inflowpayai/inflow-go)" = "$1"
    test -z "$(go list -m -f '{{if .Replace}}{{.Replace.Path}}{{end}}' github.com/inflowpayai/inflow-go)"
fi
go run .
