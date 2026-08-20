# Support setting various labels on the final image
ARG COMMIT=""
ARG VERSION=""
ARG BUILDNUM=""

# Build Geth in a stock Go builder container
FROM golang:1.25-alpine AS builder

# Pin Datadog orchestrion to a specific commit (tag v1.7.0) so the build-time
# code rewriter cannot be silently re-pointed by an upstream re-tag.
ARG ORCHESTRION_VERSION=d99388bc684cfb427e297e2f82e8b52886dce69d
# Opt in to orchestrion instrumentation for this build stage; build/ci.go
# applies it through the go toolchain wrapper when USE_ORCHESTRION is set.
ENV USE_ORCHESTRION=1

RUN apk add --no-cache gcc musl-dev linux-headers git

# Get dependencies - will also be cached if we won't change go.mod/go.sum
COPY go.mod /go-ethereum/
COPY go.sum /go-ethereum/
RUN go install github.com/DataDog/orchestrion@${ORCHESTRION_VERSION}
RUN cd /go-ethereum && go mod download

COPY . /go-ethereum
RUN cd /go-ethereum && go run build/ci.go install -static ./cmd/geth
RUN cd /go-ethereum && go run build/ci.go install -static ./cmd/bootnode
# Fail the build if geth was not actually instrumented with Datadog dd-trace-go.
RUN go tool nm /go-ethereum/build/bin/geth | grep -q 'dd-trace-go' \
    || (echo 'ERROR: geth binary is not instrumented with Datadog dd-trace-go' && exit 1)

# Pull Geth into a second stage deploy alpine container
FROM alpine:3.20

RUN apk add --no-cache ca-certificates curl bash
RUN apk add --no-cache openssl # quorum (6 may 2024): 3.1.4-r5 is the installed openssl version, want 3.1.4-r6 to get fix for CVE-2024-2511
COPY --from=builder /go-ethereum/build/bin/geth /usr/local/bin/
COPY --from=builder /go-ethereum/build/bin/bootnode /usr/local/bin/

RUN echo 'alias ga="geth attach http://localhost:8545"' >> /etc/profile

EXPOSE 8545 8546 30303 30303/udp
ENTRYPOINT ["geth"]

# Add some metadata labels to help programatic image consumption
ARG COMMIT=""
ARG VERSION=""
ARG BUILDNUM=""

LABEL commit="$COMMIT" version="$VERSION" buildnum="$BUILDNUM"
