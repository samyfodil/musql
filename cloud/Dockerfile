# musql cloud with its console and the embedded AI, built from the repository root:
#   docker build -f cloud/Dockerfile -t musql-cloud .
#   docker run --rm -it -v musql-cloud:/data musql-cloud init --admin-email you@example.com
#   docker run -p 8080:8080 -v musql-cloud:/data musql-cloud
# Pure Go, so the build cross-compiles on the build host for every platform. The
# console is static files, built once whatever the target.
FROM --platform=$BUILDPLATFORM golang:1.27 AS src
WORKDIR /src
COPY . .

# The console imports the npm package, whose wasm module is built first.
FROM src AS wasm
RUN sh npm/build.sh

FROM --platform=$BUILDPLATFORM node:22 AS console
WORKDIR /src
COPY --from=wasm /src/npm npm
COPY cloud/console cloud/console
RUN cd cloud/console && npm ci --no-audit --no-fund && npm run build

FROM src AS build
RUN cd cloud && go mod download
# Declared after the download so that one download serves every platform.
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -C cloud -tags jitllm -trimpath -ldflags="-s -w" -o /musql-cloud ./cmd/musql-cloud
RUN mkdir -p /out/data /out/tmp && chmod 1777 /out/tmp && chown 65532:65532 /out/data
# The defaults for a container, and the file `init` reads too (the working
# directory is /). Mount your own over it: public_url is the OIDC issuer.
RUN printf 'listen: ":8080"\ndata_dir: "/data"\nconsole:\n  dir: "/console"\n' > /out/config.yaml

# No UPX here, unlike the musqld image: unpacking a binary this size would slow
# every start. A static binary needs little else: the CA certificates (the AI
# providers are called over TLS), the console, /data, and /tmp, where large
# sorts spill.
FROM scratch
COPY --from=build /musql-cloud /musql-cloud
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=console /src/cloud/console/dist /console
COPY --from=build /out/ /
USER 65532:65532
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/musql-cloud"]
CMD ["-config", "/config.yaml"]
