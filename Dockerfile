FROM alpine

# Create non-root user
RUN addgroup -g 1000 fs && \
    adduser -D -u 1000 -G fs fs

COPY fs /usr/local/bin/fs
RUN chmod +x /usr/local/bin/fs

USER fs

ENTRYPOINT ["/usr/local/bin/fs"]
