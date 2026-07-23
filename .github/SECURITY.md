# Security Policy

## Reporting a vulnerability

Report vulnerabilities privately through [GitHub's private vulnerability
reporting][reporting] (the repository's **Security** tab → *Report a
vulnerability*). Please do **not** open a public issue for a suspected
vulnerability.

Include, where possible: affected version or commit, a description of the
impact, and a minimal reproduction (a request, input, or test case).

[reporting]: https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing/privately-reporting-a-security-vulnerability#privately-reporting-a-security-vulnerability

## Supported versions

Until the `v1.0.0` release, only the latest `main` receives security fixes.
After `v1.0.0`, the latest minor release line receives fixes.

## Response process

- We acknowledge a report within **3 business days**.
- We place a validated report under embargo, develop and review a fix, and
  prepare a coordinated release.
- We aim to ship a fix within **90 days** of validation; a shorter window
  applies to actively exploited or high-severity issues.
- Once a fix is released we publish a [GitHub Security Advisory][advisories]
  (requesting a CVE where warranted) crediting the reporter unless they prefer
  to remain anonymous.

[advisories]: https://github.com/go-faster/fs/security/advisories

## Dependency and code auditing

- **Dependabot** watches Go module dependencies and GitHub Actions for known
  vulnerabilities and opens update PRs.
- **govulncheck** runs in CI (`.github/workflows/govulncheck.yml`) against the
  module and the standard library, failing on any vulnerability reachable from
  the code paths we build.
- **Fuzzing** (`.github/workflows/fuzz.yml`, `make fuzz`) continuously
  exercises the untrusted-input wire parsers — SigV4 authorization and
  credential parsing, aws-chunked framing, and the XML request-body decoders —
  for panics and denial-of-service inputs. Seed corpora also run as unit tests,
  so known crashers stay fixed.
