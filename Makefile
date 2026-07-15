.PHONY: check fmt-check go-check protocol-check pwa-check e2e e2e-nix

check: fmt-check go-check protocol-check pwa-check

fmt-check:
	@test -z "$$(gofmt -l $$(git ls-files '*.go'))" || { \
		printf '%s\n' 'Go files need gofmt:'; \
		gofmt -l $$(git ls-files '*.go'); \
		exit 1; \
	}
	nixfmt --check flake.nix nix/examples/*.nix nix/modules/*.nix nix/tests/*.nix web/scripts/playwright-nixos.nix

go-check:
	go vet ./...
	go test ./...
	go test -race ./...

protocol-check:
	python3 -m unittest discover -s tests -p 'test_*.py'

pwa-check:
	cd web && npm ci --no-audit --no-fund
	cd web && npm run typecheck
	cd web && npm test
	cd web && npm run build
	git diff --exit-code -- web/src/protocol/generated-validator.ts

e2e:
	cd web && npm ci --no-audit --no-fund
	cd web && npm run test:e2e

e2e-nix:
	cd web && npm ci --no-audit --no-fund
	cd web && npm run test:e2e:nix
