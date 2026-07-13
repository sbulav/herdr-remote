.PHONY: relay-install relay-run relay-plugin test lint check ios-build

relay-install:
	pip install -r relay/requirements.txt

relay-run:
	python3 relay/herdr_relay.py

relay-plugin:
	herdr plugin link relay/

test:
	./tests/run.sh
	python3 -m unittest discover -s tests -p 'test_*.py'

lint:
	ruff check --select E9,F63,F7,F82 relay tests

check: lint test

ios-build:
	cd herdi-ios && swift build
