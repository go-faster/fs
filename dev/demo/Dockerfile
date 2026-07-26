# Images for the demo cluster, built from this checkout rather than pulled, so
# what runs is what is in the tree.
#
# Build context is the repository root.

# The admin dashboard. dist/ is not committed — it is built in CI and on
# release — so a binary built without this stage serves a "not built" page and
# the demo's whole point, the panel, would be missing.
FROM node:22-alpine AS ui

WORKDIR /src/frontend

COPY internal/adminhandler/frontend/package.json internal/adminhandler/frontend/package-lock.json ./
RUN npm ci

COPY internal/adminhandler/frontend/ ./

# build:only rather than build: the generated API client is committed and CI
# checks it has not drifted, so regenerating it here would only add a
# dependency on the OpenAPI document's path resolving inside the image.
RUN npm run build:only

# The Go binaries.
FROM golang:1.26-alpine AS build

WORKDIR /src

# Modules first: they change far less often than the code, so this layer is
# reused across every rebuild of the demo.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=ui /src/frontend/dist internal/adminhandler/frontend/dist

RUN CGO_ENABLED=0 go build -o /out/fs ./cmd/fs \
	&& CGO_ENABLED=0 go build -o /out/loadgen ./dev/demo/loadgen

# The storage node.
FROM alpine AS fs

RUN addgroup -g 1000 fs && adduser -D -u 1000 -G fs fs

# The data directory has to exist in the image, owned by the user the process
# runs as: Docker initializes an empty named volume from the mount point it
# finds, ownership included. Without this the volume arrives owned by root and
# a non-root node cannot create so much as its index directory.
RUN mkdir -p /data && chown fs:fs /data

COPY --from=build /out/fs /usr/local/bin/fs

USER fs
ENV USER=fs

ENTRYPOINT ["/usr/local/bin/fs"]

# The load generator.
FROM alpine AS loadgen

RUN addgroup -g 1000 fs && adduser -D -u 1000 -G fs fs

COPY --from=build /out/loadgen /usr/local/bin/loadgen

USER fs
ENV USER=fs

ENTRYPOINT ["/usr/local/bin/loadgen"]
