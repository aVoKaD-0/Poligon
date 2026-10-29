#!/bin/sh
set -eu
make lint
make test
make contracts
sh tests/compose-integration.sh
make build
docker compose build
sh tests/image-smoke.sh
