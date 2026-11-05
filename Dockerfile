# syntax=docker/dockerfile:1
# The build stage runs on the build host and cross-compiles, so a multi-arch
# build does not compile Go under emulation.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/anypub ./cmd/anypub

FROM alpine:3.23
# sh is needed for hooks.on_unpublish
RUN apk add --no-cache ca-certificates && adduser -D -H -u 10001 anypub \
    && mkdir -p /var/lib/anypub && chown 10001 /var/lib/anypub
COPY --from=build /out/anypub /usr/local/bin/anypub
USER 10001
ENTRYPOINT ["anypub"]
CMD ["-config", "/etc/anypub/anypub.yaml"]
