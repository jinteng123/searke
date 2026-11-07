# Multi-stage build for searke — produces a scratch image containing
# only the static binary, CA certs and a tmp dir. No registry pulls are
# needed beyond the Go toolchain image. Multi-arch via buildx:
#   docker buildx build --platform linux/amd64,linux/arm64 -t searke .

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
# GOPROXY is overridable for networks where proxy.golang.org is
# unreachable: --build-arg GOPROXY=https://proxy.golang.org,direct
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -buildid= \
      -X github.com/jint/searke/internal/cli.Version=${VERSION}" \
    -o /out/searke ./cmd/searke
# scratch has no mkdir, so prepare runtime dirs in the build stage:
#   /tmp          — searke extracts .oci.tar payloads there
#   /home/searke  — HOME for ~/.ssh/known_hosts and default key lookup
RUN mkdir -p /scratch/tmp /scratch/home/searke/.ssh && \
    chmod 1777 /scratch/tmp && chmod 700 /scratch/home/searke/.ssh

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build --chown=65532:65532 /scratch /
COPY --from=build /out/searke /usr/local/bin/searke
ENV HOME=/home/searke
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/searke"]
