.PHONY: hooks build test test-all bench tune run vet fmt clean check

check: fmt vet build test run

# keeps vendor references out of every commit; git never enables a repository hook on its own
hooks:
	@git config core.hooksPath .githooks 2>/dev/null || true

build: hooks
	go build ./...

test: hooks
	go test ./... -v

test-all: hooks
	./scripts/test.sh

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

clean:
	rm -f qkd_post_processing
	go clean
