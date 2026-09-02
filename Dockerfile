# cgo (libpg_query) requires a matching-libc build stage; distroless base carries glibc.
# The stage runs on the build host and cross-compiles with Debian's gcc-<triplet> when the target arch differs.
FROM --platform=$BUILDPLATFORM golang:1.26-trixie AS build
ARG BUILDARCH
ARG TARGETARCH
RUN if [ "$TARGETARCH" != "$BUILDARCH" ]; then \
      case "$TARGETARCH" in \
        amd64) pkgs="gcc-x86-64-linux-gnu libc6-dev-amd64-cross" ;; \
        arm64) pkgs="gcc-aarch64-linux-gnu libc6-dev-arm64-cross" ;; \
      esac; \
      apt-get update && apt-get install -y --no-install-recommends $pkgs && rm -rf /var/lib/apt/lists/*; \
    fi
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
RUN if [ "$TARGETARCH" != "$BUILDARCH" ]; then \
      case "$TARGETARCH" in amd64) export CC=x86_64-linux-gnu-gcc ;; arm64) export CC=aarch64-linux-gnu-gcc ;; esac; \
    fi; \
    CGO_ENABLED=1 GOARCH=$TARGETARCH go build -ldflags "-s -w \
      -X github.com/SamuelMolling/godwit/internal/version.Version=${VERSION} \
      -X github.com/SamuelMolling/godwit/internal/version.Commit=${COMMIT}" \
      -o /godwit ./cmd/godwit

FROM gcr.io/distroless/base-debian13:nonroot
COPY --from=build /godwit /godwit
EXPOSE 8474
ENTRYPOINT ["/godwit"]
