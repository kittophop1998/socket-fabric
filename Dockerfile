FROM golang:1.27.2-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum ./
COPY sdk/go/go.mod sdk/go/go.sum ./sdk/go/
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/socketfabric ./cmd/server
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/socketfabric-manage ./cmd/manage

FROM alpine:3.23

RUN apk add --no-cache ca-certificates \
    && addgroup -S socketfabric \
    && adduser -S -G socketfabric socketfabric
WORKDIR /app
COPY --from=builder --chown=socketfabric:socketfabric /out/socketfabric ./socketfabric
COPY --from=builder --chown=socketfabric:socketfabric /out/socketfabric-manage ./socketfabric-manage
ENV APP_ENV=production GIN_MODE=release
USER socketfabric:socketfabric
EXPOSE 8080
STOPSIGNAL SIGTERM
ENTRYPOINT ["/app/socketfabric"]
