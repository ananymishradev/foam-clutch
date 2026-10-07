# syntax=docker/dockerfile:1

# foam-clutch: `clutch` CLI + HTTP service on Slurm clusters.
# Build:  docker build --build-arg VERSION=$(git rev-parse --short HEAD) -t <user>/foam-clutch:latest .
# Run:    docker run --rm <user>/foam-clutch:latest version
#         docker run --rm -v "$PWD:/work" -w /work <user>/foam-clutch:latest it -manifest manifest.yaml

ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.Version=${VERSION}" \
      -o /out/clutch ./cmd/clutch

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=build /out/clutch /usr/local/bin/clutch
ENTRYPOINT ["clutch"]
CMD ["version"]
