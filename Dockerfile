# musqld, the Hrana server: docker run -p 8080:8080 -v musql:/data ghcr.io/samyfodil/musql
# Pure Go, so the build cross-compiles on the build host for every platform.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /musqld ./cmd/musqld
# UPX shrinks the binary from ~17 MB to ~5 MB for ~0.3 s of startup, a fine
# trade for a long-running server.
RUN apt-get update -qq && apt-get install -qq -y upx-ucl >/dev/null && upx -q --best --lzma /musqld
RUN mkdir -p /out/data /out/tmp && chmod 1777 /out/tmp && chown 65532:65532 /out/data

# A static binary needs nothing else. /tmp is where large sorts spill.
FROM scratch
COPY --from=build /musqld /musqld
COPY --from=build /out/ /
USER 65532:65532
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/musqld", "-listen", ":8080"]
CMD ["-db", "/data/app.musq"]
