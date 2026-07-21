# Automatic TLS with Let's Encrypt (ACME)

This proxy supports automatic certificate provisioning and renewal via the
ACME protocol (RFC 8555) using golang.org/x/crypto/acme/autocert.  The
HTTP-01 challenge type is used exclusively.

## Prerequisites

### DNS

Add an A record (and AAAA for IPv6) for each domain you want a certificate
for, pointing at the public IP address of the server running this proxy.

Example (using sanskarpan.xyz):

    sanskarpan.xyz.     60  IN  A  203.0.113.10
    www.sanskarpan.xyz. 60  IN  A  203.0.113.10

The names must propagate before the first TLS handshake triggers certificate
issuance, or the HTTP-01 challenge will fail.

### Port 80 — HTTP-01 challenge

Let's Encrypt validates domain ownership by fetching a token over plain HTTP
on port 80.  Port 80 must be open and reachable from the internet on the
server running this proxy.  The proxy starts a dedicated listener on the
configured `http_challenge_port` (default 80) for this purpose when `Start()`
is called — the port is not opened during config validation (`--validate`).

### Port 443 — HTTPS traffic

Port 443 must be open for the HTTPS data-plane listener.

## Staging vs. production Let's Encrypt

Always test with the staging environment first.  Staging certificates are not
trusted by browsers but the full ACME flow is exercised without consuming
production rate-limit quota.

| Environment | directory_url |
|-------------|---------------|
| Staging (test) | `https://acme-staging-v02.api.letsencrypt.org/directory` |
| Production     | `https://acme-v02.api.letsencrypt.org/directory` |

Omit `directory_url` entirely to use the autocert default (production).

## Cache directory

Certificates and account keys are cached on disk so they survive restarts.
Create the directory and grant the proxy process write access before starting:

    sudo mkdir -p /var/cache/rplb/acme
    sudo chown <proxy-user>: /var/cache/rplb/acme
    sudo chmod 700 /var/cache/rplb/acme

If `cache_dir` is empty, an in-memory cache is used; all certificates are
re-issued on every restart and you will hit rate limits quickly.

The cache directory can also be set via the `ACME_CACHE_DIR` environment
variable, which overrides the `cache_dir` value in the config file.  This is
useful for container deployments where the cache directory is injected at
runtime rather than baked into the config file:

    ACME_CACHE_DIR=/run/secrets/acme-cache proxy --config configs/config.acme.yaml

## How to run (sanskarpan.xyz example)

1. Complete the DNS and firewall prerequisites above.
2. Edit `configs/config.acme.yaml` and update `domains` to your hostnames.
3. Create the cache directory (see above).
4. Start the proxy:

       proxy --config configs/config.acme.yaml

5. The first HTTPS connection to one of the configured domains triggers
   certificate issuance.  Renewal is automatic when the certificate is within
   30 days of expiry.

## Switch to production

Once staging works, change `directory_url` in `configs/config.acme.yaml`:

    directory_url: https://acme-v02.api.letsencrypt.org/directory

Then delete the staging cache (it contains an untrusted account key) and
restart:

    rm -rf /var/cache/rplb/acme/*
    proxy --config configs/config.acme.yaml
