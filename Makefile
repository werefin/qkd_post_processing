.PHONY: hooks build test bench tune run vet fmt lint pytest check services clean

# what CI runs, on this machine: format, vet, build, race tests, lint and the Python tests
check: fmt vet build test lint pytest

# keeps vendor references out of every commit; git never enables a repository hook on its own
hooks:
	@git config core.hooksPath .githooks 2>/dev/null || true

build: hooks
	go build ./...

test: hooks
	go test -race -count=1 ./...

bench:
	go test -run '^$$' -bench . -benchmem ./...

tune:
	./scripts/tune-blocksize.sh

run:
	go run ./cmd/qkd_post_processing -n 300000 -err 0.005 -chunk 3000

vet:
	go vet ./...

fmt:
	@test -z "$$(gofmt -l .)" || (echo "not gofmt'd:"; gofmt -l .; exit 1)

lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 run ./...
	ruff check .

pytest:
	python3 -m unittest services.tests.test_services
	cd acquisition && python3 -m unittest test_live_peak test_coincidence_logger test_two_party_blocks test_two_party_stream test_clock_tracker

# every node service in one process on :9823, from the binaries built at the repository root
services:
	go build ./cmd/qkd_post_processing ./cmd/importtt
	python3 -m services all

clean:
	rm -f qkd_post_processing importtt tuneblock qkdpki
	go clean
