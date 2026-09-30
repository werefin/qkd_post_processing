.PHONY: build test test-all bench tune run vet fmt clean check

check: fmt vet build test run

build:
	go build ./...

test:
	go test ./... -v

test-all:
	./scripts/test.sh

bench:
	go test -run '^$$' -bench . -benchmem ./...

tune:
	./scripts/tune-blocksize.sh

run:
	go run ./cmd/qoj_post_processing -n 300000 -err 0.005 -chunk 3000

vet:
	go vet ./...

fmt:
	@test -z "$$(gofmt -l .)" || (echo "not gofmt'd:"; gofmt -l .; exit 1)

clean:
	rm -f qoj_post_processing
	go clean
