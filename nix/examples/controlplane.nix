{
  # This example assumes the flake's NixOS control-plane module is imported.
  # Provision every /run/secrets path outside the Nix store.
  services.herdr-controlplane = {
    enable = true;
    browserListen = "127.0.0.1:8080";
    connectorListen = ":8443";
    origin = "https://herdr.example.com";
    upstreamLogoutUrl = "https://id.example.com/logout?post_logout_redirect_uri=https%3A%2F%2Fherdr.example.com%2F";
    trustedProxyCIDRs = [
      "127.0.0.0/8"
      "::1/128"
    ];

    oidc = {
      issuer = "https://id.example.com";
      audience = "herdr-control";
      subject = "REPLACE_WITH_OPERATOR_SUBJECT";
      mfa = "REPLACE_WITH_PROVIDER_MFA_ASSURANCE";
    };

    credentials = {
      sessionSecretFile = "/run/secrets/herdr/session-secret";
      privateCaCertFile = "/run/secrets/herdr/connector-client-ca.crt";
      privateCaKeyFile = "/run/secrets/herdr/connector-client-ca.key";
      connectorTlsCertFile = "/run/secrets/herdr/connector-server.crt";
      connectorTlsKeyFile = "/run/secrets/herdr/connector-server.key";
      connectorClientCaFile = "/run/secrets/herdr/connector-client-ca.crt";
    };
  };

  # Only the mTLS listener is reachable from connector hosts. The browser
  # listener remains on loopback behind the OIDC reverse proxy.
  networking.firewall.allowedTCPPorts = [ 8443 ];
}
