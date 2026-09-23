BIN  := bin
CMDS := ratchet strata lens docket plumb judge

.PHONY: all build test race cover check arch dart clean install

all: build

## build: compile every tool into ./bin
build:
	@mkdir -p $(BIN)
	@for c in $(CMDS); do \
		go build -o $(BIN)/$$c ./cmd/$$c || exit 1; \
		echo "  $(BIN)/$$c"; \
	done

test:
	go test ./...

race:
	go test -race -count=1 ./...

## cover: the cmd/* tests drive real binaries as child processes, so plain
## `go test -cover` reports 0% for them. GOCOVERDIR collects the child counters.
cover:
	@rm -rf /tmp/assay-cov && mkdir -p /tmp/assay-cov
	@GOCOVERDIR=/tmp/assay-cov go test -count=1 ./... >/dev/null
	@echo "--- end-to-end (binaries) ---"
	@go tool covdata percent -i=/tmp/assay-cov
	@echo "--- in-process (libraries) ---"
	@go test -count=1 -coverprofile=/tmp/assay-unit.out ./internal/... ./pkg/... >/dev/null
	@go tool cover -func=/tmp/assay-unit.out | tail -1

## dart: a real DCM report, checked in from a small synthetic package, must
## import identically and hold its baseline. If DCM's format drifts, or the
## importer's fingerprints move, this is where it shows.
dart: build
	@./$(BIN)/ratchet import internal/dcm/testdata/demo/report.json --root internal/dcm/testdata/demo \
		--mode check --strict-caps

## arch: does this codebase obey the architecture it declares?
arch: build
	@./$(BIN)/plumb check .

## check: what CI runs, minus the build matrix
check: build race arch dart
	@gofmt -l . | grep -v testdata && { echo "not gofmt'd"; exit 1; } || true
	go vet ./...
	./$(BIN)/ratchet check .

install:
	@for c in $(CMDS); do go install ./cmd/$$c; done

clean:
	rm -rf $(BIN)
