# Enterprise Go backend

The enterprise backend consists of two Go commands:

- `controlplane` serves the authenticated browser API and a separate connector mTLS endpoint.
- `connector` opens an outbound WSS connection and talks to Herdr through a local Unix socket.

The checked-in [browser](browser-protocol-v1.md) and [connector](connector-protocol-v1.md) protocols remain the wire-contract authority.

## Build and test

The module targets Go 1.24 or newer.

```bash
go build ./cmd/controlplane ./cmd/connector
go test ./...
go vet ./...
go test -race ./...
```

## Dependencies

The backend uses three direct dependencies:

- `github.com/coder/websocket` provides maintained, compression-disabled WebSocket support with context-aware I/O.
- `modernc.org/sqlite` provides durable SQLite storage without CGO. This keeps connector and control-plane builds portable.
- `github.com/SherClockHolmes/webpush-go` implements VAPID signing and Web Push payload encryption. Non-2xx responses are never reported as success.

Prometheus output, TLS, certificate issuance, logging, HTTP, and the Herdr NDJSON client use the Go standard library.

## Control-plane configuration

The control plane has no implicit production secrets. It reads the session secret, CA private key, TLS private key, and VAPID private key from files.

```bash
controlplane \
  -browser-listen 127.0.0.1:8080 \
  -connector-listen :8443 \
  -origin https://herdr.example.com \
  -database /var/lib/herdr-controlplane/control.db \
  -static-dir /var/lib/herdr-controlplane/pwa \
  -session-secret-file /run/credentials/session-secret \
  -private-ca-cert-file /run/credentials/connector-ca.crt \
  -private-ca-key-file /run/credentials/connector-ca.key \
  -connector-tls-cert-file /run/credentials/server.crt \
  -connector-tls-key-file /run/credentials/server.key \
  -connector-client-ca-file /run/credentials/connector-ca.crt \
  -trusted-proxy-cidrs 127.0.0.0/8,::1/128 \
  -oidc-issuer https://id.example.com \
  -oidc-audience herdr-control \
  -oidc-subject operator-subject \
  -oidc-mfa urn:example:mfa
```

The reverse proxy must remove client-supplied copies and set exactly one value for each header:

- `X-OIDC-Issuer`
- `X-OIDC-Audience`
- `X-OIDC-Subject`
- `X-OIDC-Assurance`

The service accepts those headers only from loopback or `-trusted-proxy-cidrs`. Every value must exactly match its configured value. The proxy must terminate browser TLS and restrict direct access to `-browser-listen`.

The connector listener requires a verified client certificate during the TLS handshake. Do not expose the browser listener on that port.

Optional Web Push flags are `-vapid-public-key`, `-vapid-private-key-file`, and `-vapid-subscriber`. Push payloads contain only a random event ID and coarse event kind.

## Enrollment

An authenticated operator creates an enrollment through `POST /api/v1/enrollments`. The request requires the HTTP-only session cookie and the `X-CSRF-Token` returned by `GET /api/v1/session`.

```json
{"display_name":"workstation"}
```

The response contains a host-scoped token that expires after ten minutes. Save it in a mode `0600` file on the connector host. The token is sent in the HTTPS request body, never in a URL.

```bash
connector \
  -enroll-url https://herdr.example.com/v1/enroll \
  -enrollment-token-file /run/credentials/enrollment-token \
  -server-ca-file /etc/ssl/certs/control-plane-ca.crt \
  -key-file ~/.config/herdr-connector/client.key \
  -cert-file ~/.config/herdr-connector/client.crt
```

The connector generates the private key locally with mode `0600`. The control plane returns a 30-day client certificate and never receives the private key. The command prints the assigned host UUID.

Set `-rotate-url https://CONNECTOR_HOST:8443/v1/connectors/rotate` during normal operation. The connector rotates its certificate when fewer than seven days remain. Deleting `/api/v1/hosts/HOST_ID/credential` revokes all active credentials for the host and closes its current lease.

## Connector configuration

```bash
connector \
  -control-plane-url wss://connectors.herdr.example.com:8443/v1/connectors/ws \
  -host-id 019f64ca-1000-7000-8000-000000000002 \
  -display-name workstation \
  -cert-file ~/.local/state/herdr-connector/client.crt \
  -initial-cert-file /run/secrets/herdr/client.crt \
  -key-file ~/.config/herdr-connector/client.key \
  -server-ca-file /etc/ssl/certs/control-plane-ca.crt \
  -herdr-socket ~/.config/herdr/herdr.sock \
  -instance-id default
```

For multiple local Herdr servers, replace `-herdr-socket` and `-instance-id` with repeatable `-herdr-instance INSTANCE_ID=/absolute/socket` flags. One host lease supports at most 16 configured instances.

The connector copies `-initial-cert-file` to `-cert-file` only when the mutable certificate is absent. Rotation atomically replaces `-cert-file`, so its parent directory must be writable; the private key remains read-only. The connector exposes no listener. It reconnects with full jitter, sends WebSocket heartbeats, and rebuilds state after reconnect. It never replays an action.

Herdr 0.7.3 is always read-only. A newer Herdr version becomes write-capable only when `ping` advertises `checked_input.v1` and the inspected socket schema contains the exact atomic `agent.send_input_checked` method and preconditions.

## Operations

- `GET /healthz` reports process liveness.
- `GET /readyz` checks SQLite availability.
- `GET /metrics` returns Prometheus text format.

SQLite retains detailed metadata-only action rows for 90 days when the retention job runs. Action-ID tombstones remain after detail deletion, so a duplicate cannot become valid. Prompt excerpts, output, input text, keys, certificate bodies, and enrollment tokens are not stored in audit rows or logs.

At startup, the control plane finalizes actions left incomplete by a crash. It also owns an independent monotonic deadline for every dispatched action, so a connected but silent connector cannot retain in-flight capacity indefinitely. Connector outcomes committed before browser delivery remain available through the metadata-only action status endpoint.
