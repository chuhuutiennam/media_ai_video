# Requires GNU make. On Windows use Git Bash, WSL, or: choco install make
.PHONY: install run run-cpu test cov lint typecheck clean

## Install package and dev dependencies in editable mode
install:
	pip install -e ".[dev]"

## Run the worker (GPU delegate, default address)
run:
	python -m mediapipe_video --model checkpoint/selfie_segmenter.tflite

## Run the worker on CPU (no GPU required)
run-cpu:
	python -m mediapipe_video --model checkpoint/selfie_segmenter.tflite --delegate cpu

## Run the test suite
test:
	pytest

## Run tests with coverage report
cov:
	pytest --cov=mediapipe_video --cov-report=term-missing

## Lint and auto-fix with ruff
lint:
	ruff check src/ tests/ --fix

## Static type check with mypy
typecheck:
	mypy src/

## Remove Python cache files
clean:
	find . -type d -name __pycache__ -exec rm -rf {} + 2>/dev/null; \
	find . -name "*.pyc" -delete 2>/dev/null; \
	rm -rf .pytest_cache .mypy_cache .ruff_cache; \
	true
