# Minimal runtime image: the fs binary (from the nfpm-built apk) on Alpine.
# Kept lean and dependency-free so it builds for every released arch,
# including linux/riscv64 — unlike a build-toolchain image, whose packages
# (cosign, syft, upx, …) are not all available on riscv64.
FROM alpine

ARG TARGETPLATFORM
COPY $TARGETPLATFORM/go-faster-fs*.apk /tmp/
RUN apk add --no-cache --allow-untrusted /tmp/go-faster-fs*.apk \
	&& rm -f /tmp/go-faster-fs*.apk

# USER lets Go's user.Current() work without cgo. fs installs its own SIGTERM/
# SIGINT handlers and forks no children, so it runs correctly as PID 1 without
# an init shim.
ENV USER=fs

ENTRYPOINT ["/usr/bin/fs"]
