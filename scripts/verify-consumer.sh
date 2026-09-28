#!/bin/sh
set -eu

repository=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
consumer=$(mktemp -d)
trap 'rm -f "$consumer/main.go" "$consumer/go.mod" "$consumer/go.sum"; rmdir "$consumer"' EXIT

cp "$repository/testdata/consumer/main.go" "$consumer/main.go"
cd "$consumer"
export GOWORK=off
go mod init example.com/inflow-consumer
go mod edit -require github.com/inflowpayai/inflow-go@v0.0.0
go mod edit -replace "github.com/inflowpayai/inflow-go=$repository"
go mod tidy
go run .
