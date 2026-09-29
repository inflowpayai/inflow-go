.PHONY: consumer-smoke docs format format-check test tidy-check verify

consumer-smoke:
	sh scripts/verify-consumer.sh

docs:
	go doc .
	go doc ./mpp
	go doc ./mpp/buyer
	go doc ./mpp/seller
	go doc ./x402
	go doc ./x402/buyer
	go doc ./x402/buyer/eip7702
	go doc ./x402/seller

format:
	gofmt -w .

format-check:
	test -z "$$(gofmt -l .)"

test:
	go test -race -coverprofile=coverage.out ./...
	awk -f scripts/check-coverage.awk coverage.out

tidy-check:
	go mod tidy -diff

verify: format-check tidy-check
	go build ./...
	go vet ./...
	$(MAKE) test
	$(MAKE) docs
	$(MAKE) consumer-smoke
