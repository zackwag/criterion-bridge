# The build stage runs on the builder's own platform and cross-compiles for
# each target, so a multi-arch build never emulates the Go compiler. The
# final stage only copies files, so it needs no emulation either.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build

WORKDIR /src
RUN apk add --no-cache ca-certificates

COPY go.mod ./
COPY cmd ./cmd

ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/criterion-bridge ./cmd/criterion-bridge

FROM scratch

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/criterion-bridge /criterion-bridge

USER 65534:65534
EXPOSE 8080
HEALTHCHECK --interval=1m --timeout=10s --start-period=10s CMD ["/criterion-bridge", "healthcheck"]
ENTRYPOINT ["/criterion-bridge"]
