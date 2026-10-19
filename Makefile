COMPOSE = docker compose -f deploy/compose/docker-compose.yml
TEST_DB = postgres://edigate:edigate@127.0.0.1:55432/edigate?sslmode=disable

.PHONY: build up down test policy-test fuzz smoke chaos-dup corpus

build:
	go build -o bin/ ./cmd/...

up:
	$(COMPOSE) up -d --build --wait

down:
	$(COMPOSE) down -v

# The tests of the Rego policy (policies/*_test.rego), one or more per rule.
policy-test:
	go run github.com/open-policy-agent/opa@v1.21.2 test policies/ -v

# Starts only postgres, then the policy tests, vet, tests with the race detector and
# govulncheck. make stops at the first failing command and returns its code.
test: policy-test
	$(COMPOSE) up -d --wait postgres
	go vet ./...
	EDIGATE_TEST_DB_URL='$(TEST_DB)' go test -race -count=1 -v ./...
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

# Fuzzes the X12 parser for 60 seconds. A failing input is saved under
# internal/edi/x12/testdata/fuzz and becomes a regression test.
fuzz:
	go test -run '^$$' -fuzz FuzzParse -fuzztime 60s ./internal/edi/x12

smoke: build up
	sh scripts/smoke.sh

# Test only. Starts a stand with fault injection (eight gateway processes that keep
# dying, a mock-1c that loses answers), makes 50 orders with repeated calls, and
# checks one order per document after at least 100 restarts. It takes several
# minutes and brings the normal stand back at the end.
chaos-dup: build
	sh scripts/chaos_dup.sh

# Builds the EDI-Inject v1 corpus (seed 1) into bench/out/corpus-seed1, then checks that the
# strict parser takes every document as its label says. Without -I, because -m needs the cwd.
corpus: build
	cd bench && python3 -m edi_inject.generate --n 300 --seed 1 --out out/corpus-seed1
	python3 -I bench/tests/check_parse.py bench/out/corpus-seed1 bin/edigatectl
