#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
control=poligon/control/v1/control.proto
mapping="M${control}=poligon/environment/gen/controlv1"
protoc -I contracts/proto --go_out=. --go_opt=module=poligon/environment --go_opt="$mapping" \
  --go-grpc_out=. --go-grpc_opt=module=poligon/environment --go-grpc_opt="$mapping" \
  "$control" poligon/environment/v1/environment.proto
