FROM golang:1.27-bookworm AS builder

WORKDIR /build/

COPY . /build/

RUN go build -tags netgo -ldflags '-s -w' -o server

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y ca-certificates tzdata && rm -rf /var/lib/apt/lists/*

COPY --from=builder /build/server /usr/local/bin/server

WORKDIR /data

EXPOSE 8080

ENTRYPOINT [ "server" ]
