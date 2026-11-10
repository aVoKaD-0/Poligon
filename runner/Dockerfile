# syntax=docker/dockerfile:1
FROM golang:1.27.2-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/runner ./cmd/runner

FROM alpine:3.24
RUN apk add --no-cache ca-certificates && addgroup -S runner && adduser -S -G runner runner \
    && install -d -m 0700 -o runner -g runner /var/lib/runner
COPY --from=build /out/runner /usr/local/bin/runner
USER runner:runner
ENV RUNNER_TEMP_DIR=/var/lib/runner
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=30s --timeout=3s CMD wget -q -O /dev/null http://127.0.0.1:8091/livez || exit 1
ENTRYPOINT ["/usr/local/bin/runner"]
