# Operations runbook

This runbook covers the self-hosted control plane, PWA, and outbound connectors. Commands use the NixOS module names exported by this repository.

## Service boundaries

| Service | Listener | Trust boundary |
| --- | --- | --- |
| OIDC reverse proxy | Public HTTPS | Authenticates one operator, requires MFA, strips and sets identity headers |
| Control-plane browser API | Loopback HTTP, default `127.0.0.1:8080` | Trusts identity headers only from configured proxy CIDRs |
| Control-plane connector API | Public or private TCP, default `8443` | Requires a client certificate issued by the connector CA |
| Connector | None | Makes outbound WSS connections and reads local Herdr sockets |

Do not combine the browser and connector listeners at the reverse proxy. Do not expose a connector or Herdr socket over the network.
Keep `/metrics` private to the host or monitoring network.

## Secret inventory

Provision secrets at runtime and keep them out of Git and the Nix store.

### Control plane

- random session secret;
- connector client CA certificate and private key;
- connector listener certificate and private key;
- connector client CA trust file;
- optional VAPID private key.

### Connector

- client private key generated on the connector host;
- read-only initial enrolled client certificate;
- mutable client certificate in the connector state directory;
- connector endpoint server CA certificate;
- short-lived enrollment token, deleted immediately after use.

Restrict private keys and enrollment tokens to mode `0600`. Restrict their parent directories to mode `0700`. The service user needs read access; no other local user should have it. The daemon never writes the private key.

## Start and stop

```bash
systemctl restart herdr-controlplane
systemctl status herdr-controlplane
journalctl -u herdr-controlplane --since today
```

```bash
systemctl restart herdr-connector
systemctl status herdr-connector
journalctl -u herdr-connector --since today
```

For the Home Manager module, add `--user` to each `systemctl` and `journalctl` command. The user unit retains seccomp, address-family, personality, privilege-escalation, and resource-limit protections. It intentionally omits privileged mount, device, kernel, capability, and control-group hardening that a user manager cannot reliably enforce.

Both daemons handle `SIGTERM`. The connector reconnects with jitter after transient failures and never replays an action.

## Health and metrics

Query the browser listener locally:

```bash
curl --fail http://127.0.0.1:8080/healthz
curl --fail http://127.0.0.1:8080/readyz
curl --fail http://127.0.0.1:8080/metrics
```

- `/healthz` confirms process liveness.
- `/readyz` confirms SQLite availability.
- `/metrics` returns Prometheus text.

Alert on readiness failure, repeated connector disconnects, malformed-message growth, audit completion failures, and certificate expiry. Never add prompt, output, input, keys, tokens, or certificate bodies to logs or metric labels.

## Enrollment

1. Authenticate through the OIDC proxy.
2. Fetch `GET /api/v1/session` and retain the secure session cookie and CSRF token.
3. Send `POST /api/v1/enrollments` with `{"display_name":"HOST_LABEL"}` and the CSRF token.
4. Transfer the returned token through a protected channel into a mode `0600` file.
5. Run the connector once with `-enroll-url`, `-enrollment-token-file`, `-server-ca-file`, `-key-file`, and `-cert-file` in a private staging directory.
6. Import the key and certificate into the deployment's read-only secret provisioning system.
7. Configure the module's `credentials.initialCertFile` and `credentials.keyFile` paths.
8. Record the printed host UUID in the connector module configuration.
9. Delete the token and staging files.

Enrollment tokens expire after ten minutes and are single-use. The connector generates its private key locally. If enrollment fails, inspect sanitized service logs and create a new token rather than reusing one.

## Certificate rotation and revocation

Set `rotateUrl` to the connector listener's HTTPS `/v1/connectors/rotate` endpoint. The connector checks every 12 hours and rotates when fewer than seven days remain. NixOS stores the mutable certificate at `/var/lib/herdr-connector/client.crt`; Home Manager stores it at `$XDG_STATE_HOME/herdr-connector/client.crt`. Atomic replacement creates its temporary file in that same writable directory. The key and initial certificate remain read-only inputs.

When upgrading from the earlier module, rename `credentials.certFile` to `credentials.initialCertFile`. On first start, the connector copies that certificate into its state directory with mode `0600`. A state certificate that matches the configured private key is preserved, so an older initial copy cannot replace a valid rotated credential.

To revoke a host credential, send an authenticated, CSRF-protected request to `DELETE /api/v1/hosts/HOST_UUID/credential`. Revocation blocks future connections and closes the current host lease.

To re-enroll after revocation:

1. Stop the connector service. Do not delete its state certificate.
2. Create a new one-time enrollment and run enrollment into a private staging directory. Enrollment generates a fresh private key.
3. Replace both read-only secret inputs with the newly enrolled key and certificate.
4. Update `hostId` if enrollment assigned a different host UUID.
5. Restart the service.

At startup, the old state certificate will not match the new key. The connector verifies that the new initial certificate does match, then atomically replaces the state certificate. If the initial certificate and key do not form a valid pair, startup fails and leaves the prior state file untouched. Fix the secret generation instead of deleting state manually.

Treat a copied or unexpectedly changed connector private key as compromised. Revoke first, preserve metadata-only audit evidence, generate a new local key, and re-enroll.

## Backup and restore

Back up `/var/lib/herdr-controlplane/control.db`, the connector issuing CA material, and each connector's mutable state certificate. Encrypt backups and restrict them to the operator and backup principal. The control-plane module rejects database paths outside its state directory.

Use SQLite's online backup mechanism or stop the control plane before copying the database. Do not copy a live database file without a SQLite-consistent method.

Restore procedure:

1. stop `herdr-controlplane`;
2. restore the database and CA material with the original ownership and restrictive modes;
3. start the service;
4. verify `/readyz`;
5. confirm connectors reacquire their host leases and send fresh snapshots;
6. inspect recent metadata-only audit entries.

The database retains action-ID tombstones after detailed audit retention. Preserve them to maintain no-replay guarantees.

## Upgrades

1. Back up SQLite and CA material.
2. Update the locked flake input.
3. Run `make check`, `nix flake check`, and all three package builds.
4. Deploy the control plane first.
5. Confirm health, readiness, OIDC login, and connector compatibility.
6. Deploy connectors in small batches.

A connector restart creates new protocol state epochs and full snapshots. In-flight writes are not replayed. Read the protocol compatibility notes before skipping a connector major.

## Incident checks

### OIDC login fails

- Confirm the request reached the loopback browser listener through the configured proxy CIDR.
- Confirm the proxy removed inbound identity-header copies.
- Compare issuer, audience, subject, and assurance values byte-for-byte with the module settings.
- Confirm WebSocket upgrade headers are preserved for `/v1/browser/ws`.

### Connector cannot connect

- Validate the WSS URL has no query string credentials.
- Check server certificate names and the configured server CA.
- Check client certificate validity and revocation.
- Confirm only one connector owns the host lease.
- Confirm the service user can read its key and certificate.

### Host appears but no agents do

- Confirm the service user can open every configured Unix socket.
- Check instance IDs for duplicates and socket paths for stale runtime directories.
- Restart the connector to force a fresh reconciliation snapshot.

### Writes are disabled

Herdr 0.7.3 is intentionally read-only. A write-capable version must advertise `checked_input.v1` and provide the checked atomic method. Do not bypass this gate.

### Audit storage fails

The control plane rejects new writes when durable audit intent cannot be stored. Restore SQLite availability before attempting another action. Do not retry an action with the same ID.
