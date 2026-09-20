# Builds the two binaries and the container image for the server.
#
# The builder stage is pinned to the same Go release the module needs: the
# post-quantum key agreement uses crypto/mlkem from the standard library, which
# first shipped in Go 1.24.
FROM golang:1.24 AS builder

WORKDIR /src

# Dependencies first, so a source change does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown

RUN CGO_ENABLED=0 go build \
        -ldflags "-s -w -X main.version=${VERSION} -X main.buildTime=${BUILD_TIME} -X main.gitCommit=${COMMIT}" \
        -o /out/aethertunnel-server . \
    && CGO_ENABLED=0 go build \
        -ldflags "-s -w -X main.version=${VERSION} -X main.buildTime=${BUILD_TIME} -X main.gitCommit=${COMMIT}" \
        -o /out/aethertunnel-client ./client

# The runtime image carries no shell and no package manager, so a compromise in the
# server has nothing to run.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/aethertunnel-server /usr/local/bin/aethertunnel-server
COPY --from=builder /out/aethertunnel-client /usr/local/bin/aethertunnel-client
COPY server.toml.example /etc/aethertunnel/server.toml.example

# The server writes its audit log, bandwidth ledger and identity keys beside the
# configuration, so that directory has to be writable.
VOLUME ["/etc/aethertunnel"]

# 7001 is the control port, 7500 the dashboard and 7001/udp the DHT node.
EXPOSE 7001 7500 7001/udp

ENTRYPOINT ["/usr/local/bin/aethertunnel-server"]
CMD ["--config", "/etc/aethertunnel/server.toml"]
