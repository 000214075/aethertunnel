# Builds the two binaries and the container image for the server.
#
# The builder stage carries Go 1.25, the release the repository's checks run on.
# It is at or above every requirement: the module's own go/toolchain directives
# select 1.24.13 (crypto/mlkem, the post-quantum key agreement, needs 1.24), and
# the visitor-auth zk-SNARK's gnark declares 1.22.
FROM golang:1.25 AS builder

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
        -ldflags "-s -w -X github.com/aethertunnel/aethertunnel/pkg/clientlib.Version=${VERSION} -X github.com/aethertunnel/aethertunnel/pkg/clientlib.BuildTime=${BUILD_TIME} -X github.com/aethertunnel/aethertunnel/pkg/clientlib.GitCommit=${COMMIT}" \
        -o /out/aethertunnel-client ./client

# The state directory is built here so that it can be copied into the runtime image
# already owned by the unprivileged user that image runs as.
RUN mkdir -p /out/state \
    && echo "Audit log, bandwidth ledger and ledger signing key are written here." > /out/state/README

# The runtime image carries no shell and no package manager, so a compromise in the
# server has nothing to run.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/aethertunnel-server /usr/local/bin/aethertunnel-server
COPY --from=builder /out/aethertunnel-client /usr/local/bin/aethertunnel-client
COPY server.toml.example /etc/aethertunnel/server.toml.example

# The server writes the audit log, the bandwidth ledger and the ledger's signing key to
# relative paths by default, and this image runs as an unprivileged user, so those paths
# have to resolve to a directory that user can write.
COPY --from=builder --chown=65532:65532 /out/state /var/lib/aethertunnel
WORKDIR /var/lib/aethertunnel
VOLUME ["/var/lib/aethertunnel"]

# The ports the shipped Deployment routes: 7001 the control port, 7500 the dashboard,
# 7002/udp the xtcp rendezvous and 7003/udp the DHT node. The manifests give the DHT a
# number of its own because a Service merges its ports by number, so a TCP control port
# and a UDP DHT node sharing 7001 cannot both be published.
EXPOSE 7001 7500 7002/udp 7003/udp

ENTRYPOINT ["/usr/local/bin/aethertunnel-server"]
# The image carries the example, not a configuration: the default path is meant to be
# bind-mounted or provided by a ConfigMap, and the server refuses to start without it
# rather than coming up on a placeholder token. Copy the example to that path to run
# the image on its own.
CMD ["--config", "/etc/aethertunnel/server.toml"]
