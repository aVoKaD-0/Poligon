FROM golang:1.27.2-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /platform ./cmd/platform
FROM alpine:3.22.1
RUN apk add --no-cache ca-certificates tzdata && addgroup -S platform && adduser -S -G platform platform
USER platform
COPY --from=build /platform /usr/local/bin/platform
EXPOSE 8080 9090
ENTRYPOINT ["platform"]
