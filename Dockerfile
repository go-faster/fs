FROM alpine

# Create non-root user
RUN addgroup -g 1000 fs && \
    adduser -D -u 1000 -G fs fs

COPY fs /usr/local/bin/fs
RUN chmod +x /usr/local/bin/fs

USER fs

# Set USER environment variable for Go's user.Current() when cgo is not available
ENV USER=fs

ENTRYPOINT ["/usr/local/bin/fs"]
