.PHONY: test integration lint build contracts generate up down

test:
	go test -race ./...
integration:
	@test -n "$(TEST_DATABASE_URL)" || (echo 'TEST_DATABASE_URL is required'; exit 1)
	@test -n "$(TEST_NATS_URL)" || (echo 'TEST_NATS_URL is required'; exit 1)
	go test -race -count=1 ./...
lint:
	@test -z "$$(gofmt -l cmd internal contracts/assets.go migrations/migrate.go)"
	go vet ./...
build:
	go build -trimpath -o bin/platform ./cmd/platform
contracts:
	python3 scripts/contracts.py
	go test ./contracts ./internal/platform -run 'TestCanonical|TestAssessment|TestContract'
generate:
	protoc -I contracts/proto --go_out=. --go_opt=module=github.com/poligon/platform --go-grpc_out=. --go-grpc_opt=module=github.com/poligon/platform contracts/proto/poligon/control/v1/control.proto
	python3 scripts/contracts.py
up:
	docker compose up --build -d
down:
	docker compose down
