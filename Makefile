.PHONY: build test web docker validate
web:
	npm --prefix web ci
	npm --prefix web run build
build: web
	go build -o bin/asgard ./cmd/asgard
test:
	go test ./...
	npm --prefix web run typecheck
	npm --prefix web audit --audit-level=high
docker:
	docker compose -f deploy/compose.yaml build
validate:
	docker compose -f deploy/compose.yaml config -q
