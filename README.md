# Herdr Remote

Herdr Remote is a self-hosted control plane and mobile PWA for monitoring Herdr agents. An outbound connector links each Herdr host to the control plane without opening an inbound port on the host.

## Components

- **Control plane:** serves the authenticated PWA and browser API, stores metadata-only audit records, and accepts connectors on a separate mTLS listener.
- **Connector:** runs as the same operating-system user as Herdr and talks to its local Unix socket.
- **PWA:** uses the same-origin browser API and keeps prompt and terminal content in memory only.

Browser access requires an OIDC-aware reverse proxy. Connector access requires deployment-owned TLS and client-certificate authorities. There is no shared-token or URL-token fallback.

Herdr 0.7.3 is read-only because it does not provide checked atomic input. Status and bounded output work, but writes remain disabled until Herdr advertises `checked_input.v1`.

## Architecture

```text
phone or desktop browser
  | HTTPS + OIDC session
  v
OIDC reverse proxy
  | loopback HTTP + trusted identity headers
  v
Herdr control plane
  | WSS + mutual TLS
  v
outbound connector
  | local NDJSON Unix socket
  v
Herdr server and agent PTYs
```

The browser never connects to a host connector. The control plane never initiates a connection to a Herdr host.

## Quick start

See [QUICKSTART.md](QUICKSTART.md) for a deployment walkthrough. The checked examples are:

- [`nix/examples/controlplane.nix`](nix/examples/controlplane.nix)
- [`nix/examples/connector.nix`](nix/examples/connector.nix)

They contain placeholder identities and secret paths, not usable credentials.

## Nix outputs

```bash
nix build .#herdr-controlplane
nix build .#herdr-connector
nix build .#herdr-pwa
```

The flake exports:

- `nixosModules.controlplane`
- `nixosModules.connector`
- `homeManagerModules.connector`

Use the NixOS connector module for a system Herdr service. Use the Home Manager module when Herdr runs in the user's systemd session.

## Development

Enter the pinned toolchain and run the standard checks:

```bash
nix develop
make check
nix flake check
```

`make check` runs formatting checks, Go vet/tests/race tests, Python protocol conformance tests, and PWA type/unit/build checks. Playwright stays separate because it needs browser runtimes:

```bash
make e2e       # systems with Playwright browser dependencies installed
make e2e-nix   # reproducible NixOS FHS runner
```

## Documentation

- [Quick start](QUICKSTART.md)
- [Operations runbook](docs/operations.md)
- [Enterprise backend configuration](docs/enterprise-backend.md)
- [Connector protocol v1](docs/connector-protocol-v1.md)
- [Browser protocol v1](docs/browser-protocol-v1.md)

## Security boundary

- Keep the browser listener on loopback and expose it only through the OIDC proxy.
- Expose the connector listener only with mTLS client verification enabled.
- Provision private keys and session secrets outside the Nix store.
- Run each connector as the same user as its Herdr server.
- Treat connector host compromise as access to that user's agent privileges.

See the [operations runbook](docs/operations.md) for enrollment, rotation, backup, monitoring, and incident procedures.

## License

[AGPL-3.0-only](LICENSE)
