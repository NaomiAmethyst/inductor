.PHONY: build test vet fmt test-worker test-reference parity clean
PYTHON ?= python3

build:
	go build -trimpath -o bin/inductor ./cmd/inductor

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w cmd internal/inductor

test-worker:
	$(PYTHON) -m unittest discover -s internal/inductor/workers -p 'test_*.py'

test-reference:
	PYTHONPATH=reference/python $(PYTHON) -m pytest reference/python/tests -q

parity:
	PYTHONPATH=reference/python $(PYTHON) scripts/generate-parity.py

clean:
	rm -rf bin
