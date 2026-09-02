.PHONY: help setup lint test verify

help:
	@echo "Kelvran is pre-scaffolding — these targets are placeholders, not real commands yet."
	@echo "  make setup   - intended: bootstrap both toolchains (see scripts/README.md)"
	@echo "  make lint    - intended: lint gateway/ (Go) and evals/ (Python)"
	@echo "  make test    - intended: run gateway/ + evals/ test suites (see docs/testing/TESTING.md)"
	@echo "  make verify  - intended: run exactly what CI runs, in the same order"

setup:
	@echo "not yet scaffolded — see AGENTS.md's Stack section, gateway/ARCHITECTURE.md, evals/ARCHITECTURE.md"

lint:
	@echo "not yet scaffolded — see AGENTS.md's Conventions section"

test:
	@echo "not yet scaffolded — see AGENTS.md's Testing section and docs/testing/TESTING.md"

verify:
	@echo "not yet scaffolded — will run lint + test + buf breaking once CI exists (see scripts/README.md)"
