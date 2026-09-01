# cgo (libpg_query) requires a matching-libc build stage; distroless base carries glibc.
FROM golang:1.26-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
RUN CGO_ENABLED=1 go build -ldflags "-s -w \
      -X github.com/SamuelMolling/godwit/internal/version.Version=${VERSION} \
      -X github.com/SamuelMolling/godwit/internal/version.Commit=${COMMIT}" \
      -o /godwit ./cmd/godwit

FROM gcr.io/distroless/base-debian13:nonroot
COPY --from=build /godwit /godwit
EXPOSE 8474
ENTRYPOINT ["/godwit"]
