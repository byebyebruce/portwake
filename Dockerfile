# syntax=docker/dockerfile:1

FROM golang:1.26.4-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/portwake ./cmd/portwake

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
    docker-cli docker-cli-compose docker-cli-buildx
COPY --from=build /out/portwake /usr/local/bin/portwake
WORKDIR /data
EXPOSE 9000
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:9000/api/status || exit 1
ENTRYPOINT ["portwake"]
CMD ["--web", "0.0.0.0:9000"]
