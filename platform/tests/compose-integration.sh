#!/bin/sh
set -eu
# Dedicated test infrastructure only; no shared project state or work queues.
docker compose -p poligon-platform-tests -f tests/compose.yaml up -d --wait
trap 'docker compose -p poligon-platform-tests -f tests/compose.yaml down -v' EXIT INT TERM
TEST_DATABASE_URL='postgres://platform:test-only@localhost:55432/postgres?sslmode=disable' \
TEST_NATS_URL='nats://localhost:54222' go test -race -count=1 ./...
