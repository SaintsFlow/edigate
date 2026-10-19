COMPOSE = docker compose -f deploy/compose/docker-compose.yml
TEST_DB = postgres://edigate:edigate@127.0.0.1:55432/edigate?sslmode=disable

.PHONY: build up down test smoke

build:
	go build -o bin/ ./cmd/...

up:
	$(COMPOSE) up -d --build --wait

down:
	$(COMPOSE) down -v

# Starts only postgres, then vet, tests with the race detector and govulncheck.
# make stops at the first failing command and returns its code.
test:
	$(COMPOSE) up -d --wait postgres
	go vet ./...
	EDIGATE_TEST_DB_URL='$(TEST_DB)' go test -race -count=1 -v ./...
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

smoke: build up
	sh scripts/smoke.sh
