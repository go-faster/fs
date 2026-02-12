FROM alpine

ARG TARGETPLATFORM

RUN apk add --no-cache bash \
	build-base \
	curl \
	cosign \
	docker-cli \
	docker-cli-buildx \
	git \
	gpg \
	mercurial \
	make \
	openssh-client \
	syft \
	tini \
	upx

COPY $TARGETPLATFORM/goreleaser_*.apk /tmp/
RUN apk add --no-cache --allow-untrusted /tmp/goreleaser_*.apk

ENTRYPOINT ["/usr/bin/fs"]
