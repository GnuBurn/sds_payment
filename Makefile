COMPOSE := docker compose -f deployments/docker-compose.yml
DB_URL  ?= postgres://sds:sds@localhost:5434/sds?sslmode=disable

.PHONY: up down db run test test-integration vet logs image-arm64

up:     ## start postgres + wallet + payment in docker
	$(COMPOSE) up -d --build

down:   ## stop everything
	$(COMPOSE) down

db:     ## start only postgres (for running the service locally)
	$(COMPOSE) up -d db

run:    ## run payment locally against the docker postgres and wallet
	DATABASE_URL="$(DB_URL)" WALLET_URL="http://localhost:8080" HTTP_ADDR=":8081" go run ./cmd/payment

test:   ## unit tests (Postgres tests are skipped)
	go test ./...

test-integration: db  ## all tests, including Postgres tests against the docker postgres
	TEST_DATABASE_URL="$(DB_URL)" go test -count=1 ./...

vet:
	go vet ./...

logs:
	$(COMPOSE) logs -f payment

image-arm64:  ## build the image for the Raspberry Pi workers
	docker buildx build --platform linux/arm64 -f deployments/payment.Dockerfile -t sds-payment:arm64 .
