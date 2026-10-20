COMPOSE = docker compose -f deploy/compose/docker-compose.yml
TEST_DB = postgres://edigate:edigate@127.0.0.1:55432/edigate?sslmode=disable

.PHONY: build up down test policy-test fuzz smoke demo slots chaos-dup corpus bench-venv bench-test bench-prepare bench-smoke

build:
	go build -o bin/ ./cmd/...

up:
	$(COMPOSE) up -d --build --wait

# Removes the stand and its data, and the anchor files of the audit chain (they belong to the data of the stand).
down:
	$(COMPOSE) down -v
	rm -rf deploy/compose/stand/anchors

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

# Three scenarios run by a script (S1 an injection that reaches no effect, S2 a saga with receipts, S3
# faults without duplicates and a changed audit row found). It wipes the stand and builds it again, so
# a second run starts the same way, and prints "demo ok" with code 0. The stand stays up (make down).
# Ollama on the host is optional (one step reads a note through the quarantined model).
demo: build
	sh scripts/demo.sh

# Live check of the modes and of the slots on the running stand: four gateway processes of bin/edigate
# (two of mode full, one plain, one reqhash) with databases of their own and the mocks of the stand in
# common. Needs the stand (make up) and the Docker host name host.docker.internal.
slots: build
	sh scripts/slots.sh

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

# The Python environment of the harness (bench/harness, bench/metrics): Python 3.11 and the exact
# versions of bench/requirements.txt. Safe to run again.
bench-venv:
	python3.11 -m venv bench/.venv
	bench/.venv/bin/pip install --quiet --disable-pip-version-check -r bench/requirements.txt

# The tests of the harness and the metrics, one file at a time (each must exit with 0).
bench-test: bench-venv
	for f in bench/tests/test_harness_*.py; do echo "$$f"; bench/.venv/bin/python -I $$f || exit 1; done

# The measurement on 20 documents of the pilot corpus (seed 2, no held-out documents): fake models and one
# local Ollama model in C0 and C3, scripted faults F1 to F8. It prints the ASR and DTR tables and exits with 0
# if the checks of bench/harness/smoke.py hold. It takes tens of minutes. The stand must be able to reach
# Ollama (OLLAMA_BASE_URL, default http://127.0.0.1:11434); BENCH_SMOKE_MODEL picks the model (llama3.2:3b).
# Results stay in bench/out/ and are not committed.
bench-smoke: bench-prepare
	cd bench && .venv/bin/python -m harness.smoke --corpus out/corpus-seed2

# What every run of the harness needs: the built binaries, the stand, the Python environment and the pilot
# corpus (seed 2, made if it is missing or older than the generator).
bench-prepare: build up bench-venv bench/out/corpus-seed2/corpus.jsonl

# The pilot corpus is made again when a file of the generator is newer than it (a corpus made by an older
# generator would have older labels).
bench/out/corpus-seed2/corpus.jsonl: $(wildcard bench/edi_inject/*.py) $(wildcard bench/edi_inject/templates/*/*)
	cd bench && .venv/bin/python -m edi_inject.generate --n 300 --seed 2 --out out/corpus-seed2
