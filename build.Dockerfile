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

COPY $TARGETPLATFORM/go-faster-fs*.apk /tmp/
RUN apk add --no-cache --allow-untrusted /tmp/go-faster-fs*.apk

ENTRYPOINT ["/usr/bin/fs"]
