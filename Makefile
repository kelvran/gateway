.PHONY: help setup lint lint-gateway lint-evals lint-proto test test-gateway test-evals verify gen-proto check-proto

help:
	@echo "Kelvran — real targets as of the api/gatewayevents contract pass (see docs/agents/LOGS.md)."
	@echo "  make setup       - bootstrap both toolchains (go mod download + uv sync)"
	@echo "  make lint        - lint gateway/ (golangci-lint), evals/ (ruff), api/ (buf lint + breaking)"
	@echo "  make test        - run gateway/ + evals/ test suites (go test + pytest)"
	@echo "  make verify      - build + vet + lint + test + check-proto, both deployables — what CI runs"
	@echo "  make gen-proto   - regenerate Go/Python bindings from api/*.proto (requires buf)"
	@echo "  make check-proto - gen-proto, then fail if the committed generated code drifted"

setup:
	cd gateway && go mod download
	cd evals && uv sync

lint-gateway:
	cd gateway && go vet ./... && golangci-lint run ./... && go run github.com/fe3dback/go-arch-lint@v1.18.0 check

lint-evals:
	cd evals && ruff check . && uvx --with-editable . --from import-linter lint-imports

lint-proto:
	cd api && buf lint && buf breaking --against '../.git#branch=main,subdir=api'

lint: lint-gateway lint-evals lint-proto

test-gateway:
	cd gateway && go build ./... && go test ./...

test-evals:
	cd evals && uv run pytest tests/

test: test-gateway test-evals

gen-proto:
	cd api && buf generate

check-proto: gen-proto
	@git diff --exit-code -- gateway/api/gatewayevents evals/evals/contracts || \
		(echo "Generated code in gateway/api/gatewayevents or evals/evals/contracts is out of date with api/*.proto — run 'make gen-proto' and commit the result." && exit 1)

verify: lint test check-proto
	@echo "verify: lint + test + check-proto passed for both gateway/ and evals/"
