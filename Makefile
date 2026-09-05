SHELL := /bin/bash
PYTHON ?= python3
VERSION := $(shell awk -F'"' '/const Version/{print $$2}' internal/config/config.go)
LDFLAGS := -s -w
DIST := dist

.PHONY: all fmt validate check test python-test race build task-e2e e2e release release-verify clean
all: check build

fmt:
	gofmt -w $$(find . -name '*.go' -type f)

validate:
	$(PYTHON) scripts/validate-repo.py
	bash -n scripts/*.sh
	sh -n deploy/openwrt/*.init
	$(PYTHON) -m py_compile scripts/*.py

python-test:
	PYTHONPATH=sdk/python $(PYTHON) -m py_compile sdk/python/agentspool_sdk/*.py sdk/python/tests/*.py examples/python/*.py
	PYTHONWARNINGS=error::ResourceWarning PYTHONPATH=sdk/python $(PYTHON) -m unittest discover -s sdk/python/tests -v

test: python-test
	go test ./...

check: validate python-test
	test -z "$$(gofmt -l $$(find . -name '*.go' -type f))"
	go vet ./...
	go test ./...

race:
	go test -race ./...

build:
	mkdir -p $(DIST)
	CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/agentspool ./cmd/agentspool

task-e2e: build
	./scripts/e2e.sh ./$(DIST)/agentspool

e2e: task-e2e

release:
	./scripts/build-release.sh

release-verify:
	./scripts/verify-release.sh

clean:
	rm -rf $(DIST)/*
	touch $(DIST)/.gitkeep
