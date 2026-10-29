#!/bin/sh
set -eu
# Compose >= 2.24.4 is needed for !reset. .env must be generated beforehand.
compose() { docker compose -p poligon-platform-smoke -f compose.yaml -f tests/smoke.override.yaml "$@"; }
trap 'compose down -v' EXIT INT TERM
compose up --build -d --wait
compose exec -T platform wget -q -O - http://localhost:8080/livez
compose exec -T platform wget -q -O - http://localhost:8080/readyz
