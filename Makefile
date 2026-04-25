.PHONY: test test-e2e ci build

build:
	go build ./cmd/qifa

test:
	go test ./...

test-e2e:
	bash scripts/test-zot-e2e.sh

ci: test test-e2e
