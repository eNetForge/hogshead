# syntax=docker/dockerfile:1
#
# hogshead container image: one static binary on an empty base.
# Data, the config file and the admin socket all live in /data.

FROM --platform=$BUILDPLATFORM node:22-alpine AS ui
WORKDIR /src/ui
COPY ui/package.json ui/package-lock.json ./
RUN npm ci
COPY ui/ ./
# The build copies its output to ../internal/ui/dist, where Go embeds it.
RUN mkdir -p ../internal/ui && npm run build

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui /src/internal/ui/dist ./internal/ui/dist
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/enetforge/hogshead/internal/version.Version=${VERSION}" \
      -o /out/hogshead ./cmd/hogshead \
 && mkdir -p /out/data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/hogshead /usr/local/bin/hogshead
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV HOGSHEAD_CONFIG=/data/config.toml \
    HOGSHEAD_DATA_DIR=/data
VOLUME /data
EXPOSE 9000
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD ["/usr/local/bin/hogshead", "status"]
ENTRYPOINT ["/usr/local/bin/hogshead"]
# On the first start this creates /data/config.toml and the admin credentials
# and prints them once to the container log.
CMD ["serve", "--init"]
