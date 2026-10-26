# Design: virtual-host-style addressing

**Status:** proposed. Required by [COMPATIBILITY.md](../../COMPATIBILITY.md)
("Planned: each requires a design document before commitment").

**Scope:** requests that name the bucket in the `Host` header
(`https://photos.s3.example.com/2024/cat.jpg`) rather than in the path
(`https://s3.example.com/photos/2024/cat.jpg`), on the same listener as
path-style.

## Problem

fs only understands path-style addressing. A request to
`photos.s3.example.com/2024/cat.jpg` is read as bucket `2024`, key `cat.jpg`.
Every client has to be told to force path-style:

- aws-sdk-go-v2 `UsePathStyle`;
- boto3 `addressing_style='path'`;
- aws-cli `addressing_style = path`;
- rclone `force_path_style`;
- mc `--path on`.

Some clients default to virtual-host style, and some cannot be switched at
all: tools that build URLs from a bucket hostname, and browser uploads to a
bucket host. AWS announced path-style's deprecation in 2019 (and has since
postponed it), and new tooling increasingly assumes virtual-host style.

## S3 semantics

- **The bucket is the `Host` label before the service domain:**
  `<bucket>.<domain>`. The path is then the key alone (`/2024/cat.jpg`), and
  `/` addresses the bucket itself (list, `?versioning` and so on).
- **The bare domain** (`s3.example.com`) is the service endpoint:
  `GET /` lists buckets, and path-style requests keep working there.
- **SigV4 signs the request as sent.** The canonical URI is the path on the
  wire, without the bucket, and the `Host` header, bucket included, is a
  signed header. Presigned URLs carry the bucket host.
- **Bucket names with dots** (`my.photos`) work over HTTP. Over HTTPS they
  don't match a `*.domain` wildcard certificate. AWS has the same limit,
  and most SDKs fall back to path-style for such buckets over HTTPS.

## Design

### Configuration

```yaml
server:
  domains: ["s3.example.com", "s3.internal"]
```

- **`server.domains`** lists the service domains, also available as
  `server.Config.Domains` and `handler.WithDomains`.
- **Empty** (the default) means pure path-style, exactly as today: nothing
  changes for a deployment that doesn't set it.
- **Validation:** each entry is lowercased and must be a hostname without a
  port, scheme or wildcard. A bad entry fails startup.

### Resolving the host

`resolveHost(domains, host) (bucket string, ok bool)`:

1. Strip the port, and lowercase the rest. An IP literal, IPv6 in brackets
   included, never matches.
2. Find the longest configured domain the host ends with, at a label
   boundary (`.` + domain). The longest wins, so `s3.example.com` beats
   `example.com` for `b.s3.example.com`.
3. The labels before it are the bucket, dots included. A host equal to a
   domain, or one matching none, is path-style.

**A host that matches no domain is served path-style.** It is not refused,
for three reasons, and MinIO does the same:

- clients that reach the server by IP or by an internal name keep working
  when `domains` is set;
- the health checks of load balancers keep working;
- a wrong `domains` entry shows up as path-style misreads in the logs. A
  refusal would take everything down at once.

### Rewriting at the edge

A middleware, `virtualHosts`, sits just inside the request-ID middleware,
before CORS, auth and the router. When the host names a bucket, it rewrites
the request to its path-style form:

```
Host: photos.s3.example.com   GET /2024/cat.jpg
  →  URL.Path    = "/photos/2024/cat.jpg"
     URL.RawPath = "/photos" + RawPath, when RawPath is set
```

From there every layer sees a path-style request:

- the router, the ~50 handlers that cut the bucket from the path, auth's
  `requestScope` and grants, owner isolation, per-bucket CORS, and the error
  body's `<BucketName>`.

Bucket names need no escaping, because they are lowercase letters, digits,
`-` and `.`. So the rewrite cannot change how the key decodes.

**SigV4 is the one reader that needs the request as sent.** The middleware
records the original escaped path on the context
(`sigv4.WithSignedPath(ctx, path)`), and `canonicalURI` prefers it over
`URL.EscapedPath()`. That one change makes all three signature forms work
unchanged:

- header signatures;
- presigned URLs;
- streaming signatures (`aws-chunked`), whose seed signature covers the
  canonical request.

**Rejected alternative: carry the bucket in the context and leave the URL
alone.** This was the plan in `findings/DESIGN.md` §6.4.
- Every place that reads the bucket from the path would have to call a
  helper instead. That is about 50 places today, and every handler written
  later.
- A missed place fails silently and dangerously. It reads the key's first
  segment as the bucket, and could authorize or act on the wrong bucket.
- With the rewrite, the risk sits in one function and in the one reader
  that needs the original, SigV4, which a test pins.

**Other users of the original path:**
- **Request log:** the line gains `host` and the path as sent, so an
  operator sees what the client asked for.
- **`<Resource>` in error bodies:** fs shows the rewritten path, which
  names the bucket and the key. What AWS shows for a virtual-host request is
  not documented. No client is known to parse `<Resource>`, so it stays the
  rewritten path.

### Health and readiness

`server.New` answers `/health` and `/ready` before the S3 handler. With
virtual hosts, `GET photos.s3.example.com/health` is the object `health` in
bucket `photos`, not a health check. So the health routes are answered only
for requests that the host doesn't route to a bucket. The resolver runs
before the health switch, and leaves a mark on the context for it.

### What does not change

- **Path-style** works on every host, including the bare domains.
- **`x-amz-copy-source`** is always `bucket/key`, so `CopyObject` and
  `UploadPartCopy` are unaffected.
- **Bucket naming rules.** An invalid name in the host fails exactly as it
  would in the path (`InvalidBucketName` on create, `NoSuchBucket`
  otherwise).
- **The cluster.** Each node resolves on its own, and peer traffic is not
  S3.

### Proxies

The `Host` header is what is resolved. A reverse proxy must pass it through.
nginx needs `proxy_set_header Host $host`; Caddy and Traefik pass it by
default. SigV4 also signs `Host`, so a proxy that rewrites it already breaks
authentication today. `X-Forwarded-Host` is not trusted, for the same
reason. `docs/DEPLOYMENT.md` gets a section on DNS (a wildcard record per
domain), the wildcard certificate, and proxies.

## Observability

- **No metric.** Addressing style is not a way to fall behind.
- **Logs:** the request log line gains `host` and, for a virtual-host
  request, the resolved `bucket`. A misconfigured domain then shows as
  requests with hosts that resolve to nothing.
- **Startup:** the server logs the configured domains at startup.

## Admin API

Nothing. The domains are static configuration, which the operator owns and
the startup log reports.

## Testing

- **Resolver table** (`internal/core/handler`):
  - a host with a port, in mixed case, as an IPv6 literal, or as an IPv4
    address;
  - a bucket with dots;
  - overlapping domains (the longest wins);
  - the bare domain, a host matching nothing, and an empty label
    (`.s3.example.com`).
- **Rewrite:**
  - keys with `%2F`, spaces, `//` and a trailing `/`;
  - `GET /` on a bucket host lists the bucket;
  - `PUT /` on a bucket host creates the bucket;
  - a path-style request on a bare domain is untouched.
- **SigV4:** header, presigned and streaming signatures verified for a
  virtual-host request, signed by the real aws-sdk-go-v2 signer. A test with
  `WithSignedPath` removed shows the signatures fail.
- **Server:** `/health` on a bucket host reaches the object; on the bare
  domain it is the health check.
- **Integration** (`integration/`):
  - aws-sdk-go-v2 with `UsePathStyle: false` and minio-go with
    `BucketLookupDNS`, against domain `localhost`. A custom dialer sends
    every `*.localhost` to the test server, so no DNS is needed.
  - A round trip of create, put, get, list, multipart, delete and a
    presigned GET.
- **CLI smoke:** an aws-cli variant with `addressing_style = virtual`
  against `*.localhost`. On the GitHub runners systemd-resolved resolves
  `*.localhost` to loopback.
- **s3-tests:** s3t always addresses path-style (`internal/client`). A
  virtual-host run needs an option in go-faster/s3t first. That is a
  follow-up in that repo, not part of this change; the integration tests are
  the evidence here.

## Open questions

None that block implementation. The fallback to path-style for unmatched
hosts is the one behavior choice an operator might want to change (a
`strict` option that refuses them). That can wait until someone asks.

## Not in this design

- **Static website hosting** (`<bucket>.s3-website.<domain>`) is a separate
  Planned item. When it lands, its domains must be disjoint from
  `server.domains`.
- **ACME and automatic TLS** are separate too. Until then, the wildcard
  certificate is the operator's to provide.
- **Custom domains per bucket** (a `CNAME` named like the bucket, which S3
  supports over HTTP) are out. A host is matched only against
  `server.domains`.
