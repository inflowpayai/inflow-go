.PHONY: consumer-smoke docs format format-check test tidy-check verify

consumer-smoke:
	sh scripts/verify-consumer.sh

docs:
	go doc .

format:
	gofmt -w .

format-check:
	test -z "$$(gofmt -l .)"

test:
	go test -race -coverprofile=coverage.out ./...

tidy-check:
	go mod tidy -diff

verify: format-check tidy-check
	go build ./...
	go vet ./...
	$(MAKE) test
	$(MAKE) docs
	$(MAKE) consumer-smoke
