.PHONY: help setup lint lint-gateway lint-evals test test-gateway test-evals verify

help:
	@echo "Kelvran — real targets as of the test-expansion pass (see docs/agents/LOGS.md)."
	@echo "  make setup   - bootstrap both toolchains (go mod download + uv sync)"
	@echo "  make lint    - lint gateway/ (golangci-lint) and evals/ (ruff)"
	@echo "  make test    - run gateway/ + evals/ test suites (go test + pytest)"
	@echo "  make verify  - build + vet + lint + test, both deployables — what CI runs"
	@echo ""
	@echo "Not yet real: buf breaking (no api/ .proto files exist yet — see api/README.md)."

setup:
	cd gateway && go mod download
	cd evals && uv sync

lint-gateway:
	cd gateway && go vet ./... && golangci-lint run ./...

lint-evals:
	cd evals && ruff check .

lint: lint-gateway lint-evals

test-gateway:
	cd gateway && go build ./... && go test ./...

test-evals:
	cd evals && uv run pytest tests/

test: test-gateway test-evals

verify: lint test
	@echo "verify: lint + test passed for both gateway/ and evals/"
