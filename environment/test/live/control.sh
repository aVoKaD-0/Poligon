#!/bin/sh
set -eu
: "${TEST_DATABASE_URL:?disposable PostgreSQL admin URL required}"
: "${TEST_S3_ENDPOINT:?local MinIO host:port required}"
: "${TEST_S3_ACCESS_KEY:?test storage credentials required}"
: "${TEST_S3_SECRET_KEY:?test storage credentials required}"
cd "$(dirname "$0")/../.."
env_dir=$(pwd)
probe_dir=$(mktemp -d)
trap 'rm -rf "$probe_dir"' EXIT
rev=2fe2700620c946e236377c79886e511743d7608c
# PLATFORM_REPO can also point to a bare clone containing the pinned commit.
git -C "${PLATFORM_REPO:-..}" archive -o "$probe_dir/platform.tar" "$rev" platform
tar -xf "$probe_dir/platform.tar" -C "$probe_dir"
export ENVIRONMENT_PROBE_BINARY="$probe_dir/environment.test" ENVIRONMENT_PROBE_DIR="$env_dir/internal/integration"
export ENVIRONMENT_SERVER_BINARY="$probe_dir/environment-server"
go build -o "$ENVIRONMENT_SERVER_BINARY" ./cmd/server
go test -c -o "$ENVIRONMENT_PROBE_BINARY" ./internal/integration
cp test/live/platform_probe_test.go.txt "$probe_dir/platform/internal/platform/environment_provider_test.go"
cd "$probe_dir/platform"
go test -count=1 -v ./internal/platform -run '^TestEnvironmentProvider$'
