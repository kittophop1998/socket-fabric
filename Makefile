VERSION ?= dev
COMMIT ?= unknown
BUILD_TIME ?= unknown
CLI_LDFLAGS = -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildTime=$(BUILD_TIME)
export VERSION COMMIT BUILD_TIME SOURCE_DATE_EPOCH

.PHONY: cli cli-cross cli-dist release-verify release-check management-test contracts-test test-tools pr-check integration-check security-check benchmark
cli:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(CLI_LDFLAGS)' -o bin/socketfabric ./cmd/socketfabric

cli-cross:
	@set -e; for target_os in linux darwin windows; do \
		for target_arch in amd64 arm64; do \
			suffix=''; if [ "$$target_os" = windows ]; then suffix='.exe'; fi; \
			CGO_ENABLED=0 GOOS=$$target_os GOARCH=$$target_arch go build -trimpath \
				-ldflags '$(CLI_LDFLAGS)' -o bin/socketfabric-$$target_os-$$target_arch$$suffix ./cmd/socketfabric; \
		done; \
	done

# Local artifacts only. Existing cli/cli-cross commands remain available.
cli-dist:
	node scripts/release.mjs build

release-verify:
	node scripts/release.mjs verify

release-check:
	node scripts/check.mjs

# Disposable PostgreSQL fixture; requires Docker and NATS_SERVER_BIN.
management-test:
	node tests/management/run.mjs

contracts-test:
	npm ci --prefix tests/contracts --ignore-scripts --no-audit --no-fund
	go test -count=1 -tags contracttests ./internal/adapters/inbound/http ./internal/testsupport/contracts
	node tests/contracts/run.mjs
	node tests/management/run.mjs --contracts-only

test-tools:
	node scripts/test-tools.mjs --install

pr-check:
	node scripts/check.mjs --suite fast

integration-check:
	node scripts/check.mjs --suite integration

security-check:
	node scripts/check.mjs --suite security

benchmark:
	node scripts/benchmark.mjs
