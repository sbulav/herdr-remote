# Self-hosted quick start

This guide deploys one control plane and one outbound connector. It assumes you already operate an OIDC reverse proxy and a private certificate authority.

## Prerequisites

- Nix with flakes enabled on Linux
- a public HTTPS origin for the PWA, such as `https://herdr.example.com`
- an OIDC proxy that enforces MFA and can set trusted identity headers
- a separate DNS name and server certificate for the connector mTLS listener
- a private connector client CA
- Herdr running with a Unix socket

Do not place private keys, session secrets, or enrollment tokens in a Nix file or the Nix store.

## 1. Add the flake input

```nix
{
  inputs.herdr-remote.url = "github:dcolinmorgan/herdr-remote";

  outputs = { nixpkgs, herdr-remote, ... }: {
    nixosConfigurations.control = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [
        herdr-remote.nixosModules.controlplane
        ./nix/examples/controlplane.nix
      ];
    };
  };
}
```

Copy the [control-plane example](nix/examples/controlplane.nix), replace every `example.com` value, and provision the referenced files under `/run/secrets/herdr`.

## 2. Configure the OIDC proxy

Proxy the public origin to `127.0.0.1:8080`. The proxy must:

1. authenticate the user and require MFA;
2. remove client-supplied copies of all four identity headers;
3. set exactly one `X-OIDC-Issuer`, `X-OIDC-Audience`, `X-OIDC-Subject`, and `X-OIDC-Assurance` value;
4. support WebSocket upgrades for `/v1/browser/ws`;
5. keep the upstream connection on loopback or another explicitly configured trusted CIDR.

Keep `/metrics` private to the host or monitoring network; do not publish it through the PWA origin.

The four values must exactly match `services.herdr-controlplane.oidc`. Never expose the browser listener directly.

Expose TCP port `8443` for connectors. That listener performs its own TLS handshake and requires a client certificate. Do not route browser traffic to it.

## 3. Start the control plane

Rebuild the server, then verify local health and readiness:

```bash
sudo nixos-rebuild switch --flake .#control
curl --fail http://127.0.0.1:8080/healthz
curl --fail http://127.0.0.1:8080/readyz
```

Open the public origin and confirm your OIDC proxy completes a session. A direct request without trusted identity headers must not create an authenticated session.

## 4. Create a one-time enrollment

From an authenticated browser session, send `POST /api/v1/enrollments` with the session cookie, the `X-CSRF-Token` returned by `GET /api/v1/session`, and this body:

```json
{"display_name":"workstation"}
```

Store the returned token in a mode `0600` file on the connector host. It expires after ten minutes and is valid once.

## 5. Enroll the connector

Build the connector on its host, then generate its private key and certificate locally:

```bash
nix build .#herdr-connector
install -d -m 0700 "$HOME/.config/herdr-connector"
result/bin/herdr-connector \
  -enroll-url https://herdr.example.com/v1/enroll \
  -enrollment-token-file /run/secrets/herdr/enrollment-token \
  -server-ca-file /run/secrets/herdr/browser-server-ca.crt \
  -key-file "$HOME/.config/herdr-connector/enrolled-client.key" \
  -cert-file "$HOME/.config/herdr-connector/enrolled-client.crt"
```

The command prints the assigned host UUID. Put that value in `services.herdr-connector.hostId`. Import the generated key and certificate into your secret provisioning system as `client.key` and `client.crt`, then delete the enrollment token and staging copies.

## 6. Start the connector

Copy either the [NixOS connector example](nix/examples/connector.nix) or equivalent Home Manager settings. Ensure the service user can read the initial certificate and private key and can access every configured Herdr socket.

The modules never rotate the certificate in `/run/secrets`. On first start, the NixOS module copies `credentials.initialCertFile` to `/var/lib/herdr-connector/client.crt`; later rotations atomically replace that writable state file. The Home Manager module uses `$XDG_STATE_HOME/herdr-connector/client.crt`, normally `~/.local/state/herdr-connector/client.crt`. The private key remains read-only.

```bash
sudo nixos-rebuild switch --flake .#workstation
systemctl status herdr-connector
journalctl -u herdr-connector -f
```

For Home Manager, import `herdr-remote.homeManagerModules.connector` and use the same `services.herdr-connector` options. Its default socket is `~/.config/herdr/herdr.sock`. Inspect it with `systemctl --user status herdr-connector`.

## 7. Verify the connection

Open the PWA at the public origin. The host and its Herdr instances should appear after the connector sends its first snapshot.

With Herdr 0.7.3, verify status and output but expect all write controls to remain disabled. This is the safe compatibility mode, not an installation error.

Continue with the [operations runbook](docs/operations.md) before relying on the service.
