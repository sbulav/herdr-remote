{
  self,
  system,
  pkgs,
  nixpkgs,
  home-manager,
}:
let
  inherit (pkgs) lib;
  controlplaneConfig = {
    services.herdr-controlplane = {
      enable = true;
      origin = "https://herdr.example.com";
      upstreamLogoutUrl = "https://id.example.com/logout?post_logout_redirect_uri=https%3A%2F%2Fherdr.example.com%2F";
      oidc = {
        issuer = "https://id.example.com";
        audience = "herdr-control";
        subject = "operator-subject";
        mfa = "urn:example:mfa";
      };
      credentials = {
        sessionSecretFile = "/run/secrets/herdr/session-secret";
        privateCaCertFile = "/run/secrets/herdr/connector-ca.crt";
        privateCaKeyFile = "/run/secrets/herdr/connector-ca.key";
        connectorTlsCertFile = "/run/secrets/herdr/connector.crt";
        connectorTlsKeyFile = "/run/secrets/herdr/connector.key";
        connectorClientCaFile = "/run/secrets/herdr/connector-client-ca.crt";
      };
      vapid = {
        publicKey = "test-public-key";
        privateKeyFile = "/run/secrets/herdr/vapid-private-key";
        subscriber = "mailto:ops@example.com";
      };
    };
  };
  connectorConfig = {
    services.herdr-connector = {
      enable = true;
      user = "herdr";
      controlPlaneUrl = "wss://connectors.herdr.example.com:8443/v1/connectors/ws";
      hostId = "019f64ca-1000-7000-8000-000000000002";
      displayName = "workstation";
      rotateUrl = "https://connectors.herdr.example.com:8443/v1/connectors/rotate";
      credentials = {
        initialCertFile = "/run/secrets/herdr/client.crt";
        keyFile = "/run/secrets/herdr/client.key";
        serverCaFile = "/run/secrets/herdr/server-ca.crt";
      };
      instances = [
        {
          id = "default";
          socket = "/home/herdr/.config/herdr/herdr.sock";
        }
        {
          id = "work";
          socket = "/home/herdr/.config/herdr/work.sock";
        }
      ];
    };
  };

  nixosControlplane = nixpkgsSystem [
    self.nixosModules.controlplane
    controlplaneConfig
  ];
  nixosConnector = nixpkgsSystem [
    self.nixosModules.connector
    connectorConfig
    {
      users.users.herdr = {
        isSystemUser = true;
        group = "herdr";
      };
      users.groups.herdr = { };
    }
  ];
  nixpkgsSystem = modules: nixpkgs.lib.nixosSystem { inherit system modules; };

  hmConnector = home-manager.lib.homeManagerConfiguration {
    inherit pkgs;
    modules = [
      self.homeManagerModules.connector
      {
        services.herdr-connector = builtins.removeAttrs connectorConfig.services.herdr-connector [
          "instances"
          "user"
        ];
      }
      {
        home = {
          username = "herdr";
          homeDirectory = "/home/herdr";
          stateVersion = "24.11";
        };
      }
    ];
  };

  controlService = nixosControlplane.config.systemd.services.herdr-controlplane.serviceConfig;
  connectorService = nixosConnector.config.systemd.services.herdr-connector.serviceConfig;
  hmService = hmConnector.config.systemd.user.services.herdr-connector.Service;

  invalidDatabaseEvaluation = builtins.tryEval (
    builtins.deepSeq ((nixpkgsSystem [
      self.nixosModules.controlplane
      (lib.recursiveUpdate controlplaneConfig {
        services.herdr-controlplane.database = "/var/lib/herdr-controlplane/..";
      })
    ]).config.systemd.services.herdr-controlplane.serviceConfig.ExecStart
    ) true
  );
  invalidLogoutSchemeEvaluation = builtins.tryEval (
    builtins.deepSeq ((nixpkgsSystem [
      self.nixosModules.controlplane
      (lib.recursiveUpdate controlplaneConfig {
        services.herdr-controlplane.upstreamLogoutUrl = "http://id.example.com/logout";
      })
    ]).config.systemd.services.herdr-controlplane.serviceConfig.ExecStart
    ) true
  );
  invalidLogoutUserinfoEvaluation = builtins.tryEval (
    builtins.deepSeq ((nixpkgsSystem [
      self.nixosModules.controlplane
      (lib.recursiveUpdate controlplaneConfig {
        services.herdr-controlplane.upstreamLogoutUrl = "https://user@id.example.com/logout";
      })
    ]).config.systemd.services.herdr-controlplane.serviceConfig.ExecStart
    ) true
  );
  invalidConnectorUrlEvaluation = builtins.tryEval (
    builtins.deepSeq ((nixpkgsSystem [
      self.nixosModules.connector
      (lib.recursiveUpdate connectorConfig {
        services.herdr-connector.controlPlaneUrl = "wss://user@example.com/ws?token=secret";
      })
    ]).config.systemd.services.herdr-connector.serviceConfig.ExecStart
    ) true
  );
  invalidHostIdEvaluation = builtins.tryEval (
    builtins.deepSeq ((nixpkgsSystem [
      self.nixosModules.connector
      (lib.recursiveUpdate connectorConfig {
        services.herdr-connector.hostId = "not-a-uuid";
      })
    ]).config.systemd.services.herdr-connector.serviceConfig.ExecStart
    ) true
  );
  invalidDisplayNameEvaluation = builtins.tryEval (
    builtins.deepSeq ((nixpkgsSystem [
      self.nixosModules.connector
      (lib.recursiveUpdate connectorConfig {
        services.herdr-connector.displayName = lib.concatStrings (lib.replicate 81 "x");
      })
    ]).config.systemd.services.herdr-connector.serviceConfig.ExecStart
    ) true
  );
  invalidVersionEvaluation = builtins.tryEval (
    builtins.deepSeq ((nixpkgsSystem [
      self.nixosModules.connector
      (lib.recursiveUpdate connectorConfig {
        services.herdr-connector.version = "latest";
      })
    ]).config.systemd.services.herdr-connector.serviceConfig.ExecStart
    ) true
  );
  invalidWritableCredentialRejected =
    lib.any (assertion: !assertion.assertion)
      (nixpkgsSystem [
        self.nixosModules.connector
        (lib.recursiveUpdate connectorConfig {
          services.herdr-connector.credentials.keyFile = "/var/lib/herdr-connector/client.key";
        })
      ]).config.assertions;

  expectedControlExec = lib.escapeShellArgs [
    "${self.packages.${system}.herdr-controlplane}/bin/herdr-controlplane"
    "-browser-listen"
    "127.0.0.1:8080"
    "-connector-listen"
    ":8443"
    "-origin"
    "https://herdr.example.com"
    "-upstream-logout-url"
    "https://id.example.com/logout?post_logout_redirect_uri=https%3A%2F%2Fherdr.example.com%2F"
    "-database"
    "/var/lib/herdr-controlplane/control.db"
    "-static-dir"
    (toString self.packages.${system}.herdr-pwa)
    "-session-secret-file"
    "/run/secrets/herdr/session-secret"
    "-private-ca-cert-file"
    "/run/secrets/herdr/connector-ca.crt"
    "-private-ca-key-file"
    "/run/secrets/herdr/connector-ca.key"
    "-connector-tls-cert-file"
    "/run/secrets/herdr/connector.crt"
    "-connector-tls-key-file"
    "/run/secrets/herdr/connector.key"
    "-connector-client-ca-file"
    "/run/secrets/herdr/connector-client-ca.crt"
    "-trusted-proxy-cidrs"
    "127.0.0.0/8,::1/128"
    "-oidc-issuer"
    "https://id.example.com"
    "-oidc-audience"
    "herdr-control"
    "-oidc-subject"
    "operator-subject"
    "-oidc-mfa"
    "urn:example:mfa"
    "-vapid-public-key"
    "test-public-key"
    "-vapid-private-key-file"
    "/run/secrets/herdr/vapid-private-key"
    "-vapid-subscriber"
    "mailto:ops@example.com"
  ];
  expectedNixosConnectorExec = lib.escapeShellArgs [
    "${self.packages.${system}.herdr-connector}/bin/herdr-connector"
    "-control-plane-url"
    "wss://connectors.herdr.example.com:8443/v1/connectors/ws"
    "-host-id"
    "019f64ca-1000-7000-8000-000000000002"
    "-display-name"
    "workstation"
    "-version"
    "1.0.0"
    "-cert-file"
    "/var/lib/herdr-connector/client.crt"
    "-initial-cert-file"
    "/run/secrets/herdr/client.crt"
    "-key-file"
    "/run/secrets/herdr/client.key"
    "-server-ca-file"
    "/run/secrets/herdr/server-ca.crt"
    "-rotate-url"
    "https://connectors.herdr.example.com:8443/v1/connectors/rotate"
    "-herdr-instance"
    "default=/home/herdr/.config/herdr/herdr.sock"
    "-herdr-instance"
    "work=/home/herdr/.config/herdr/work.sock"
  ];
  expectedHmConnectorExec = lib.escapeShellArgs [
    "${self.packages.${system}.herdr-connector}/bin/herdr-connector"
    "-control-plane-url"
    "wss://connectors.herdr.example.com:8443/v1/connectors/ws"
    "-host-id"
    "019f64ca-1000-7000-8000-000000000002"
    "-display-name"
    "workstation"
    "-version"
    "1.0.0"
    "-cert-file"
    "/home/herdr/.local/state/herdr-connector/client.crt"
    "-initial-cert-file"
    "/run/secrets/herdr/client.crt"
    "-key-file"
    "/run/secrets/herdr/client.key"
    "-server-ca-file"
    "/run/secrets/herdr/server-ca.crt"
    "-rotate-url"
    "https://connectors.herdr.example.com:8443/v1/connectors/rotate"
    "-herdr-instance"
    "default=/home/herdr/.config/herdr/herdr.sock"
  ];
  expectedHmConnectorExecStartPre = lib.escapeShellArgs [
    "${pkgs.coreutils}/bin/install"
    "-d"
    "-m"
    "0700"
    "/home/herdr/.local/state/herdr-connector"
  ];

  checkedText =
    name: assertions: value:
    let
      checked = lib.foldl' (
        result: condition:
        assert condition;
        result
      ) value assertions;
    in
    pkgs.writeText name (builtins.toJSON checked);
in
{
  nixos-controlplane-module =
    checkedText "nixos-controlplane-module.json"
      [
        (controlService.ExecStart == expectedControlExec)
        (controlService.NoNewPrivileges == true)
        (controlService.ProtectSystem == "strict")
        (controlService.ProtectHome == true)
        (
          controlService.RestrictAddressFamilies == [
            "AF_INET"
            "AF_INET6"
            "AF_UNIX"
          ]
        )
        (controlService.UMask == "0077")
        (!invalidDatabaseEvaluation.success)
        (!invalidLogoutSchemeEvaluation.success)
        (!invalidLogoutUserinfoEvaluation.success)
      ]
      {
        execStart = controlService.ExecStart;
        hardeningChecked = true;
      };

  nixos-connector-module =
    checkedText "nixos-connector-module.json"
      [
        (connectorService.ExecStart == expectedNixosConnectorExec)
        (connectorService.NoNewPrivileges == true)
        (connectorService.ProtectSystem == "strict")
        (connectorService.ProtectHome == "read-only")
        (
          connectorService.RestrictAddressFamilies == [
            "AF_INET"
            "AF_INET6"
            "AF_UNIX"
          ]
        )
        (connectorService.UMask == "0077")
        (connectorService.StateDirectory == "herdr-connector")
        (connectorService.StateDirectoryMode == "0700")
        (connectorService.ReadWritePaths == [ "/var/lib/herdr-connector" ])
        (
          connectorService.ReadOnlyPaths == [
            "/run/secrets/herdr/client.crt"
            "/run/secrets/herdr/client.key"
            "/run/secrets/herdr/server-ca.crt"
          ]
        )
        (!invalidConnectorUrlEvaluation.success)
        (!invalidHostIdEvaluation.success)
        (!invalidDisplayNameEvaluation.success)
        (!invalidVersionEvaluation.success)
        invalidWritableCredentialRejected
      ]
      {
        execStart = connectorService.ExecStart;
        hardeningChecked = true;
      };

  home-manager-connector-module =
    checkedText "home-manager-connector-module.json"
      [
        (hmService.ExecStart == [ expectedHmConnectorExec ])
        (hmService.ExecStartPre == expectedHmConnectorExecStartPre)
        (hmService.NoNewPrivileges == true)
        (!(hmService ? ProtectSystem))
        (!(hmService ? ProtectHome))
        (!(hmService ? ProtectKernelTunables))
        (!(hmService ? CapabilityBoundingSet))
        (!(hmService ? PrivateDevices))
        (!(hmService ? ProtectControlGroups))
        (
          hmService.RestrictAddressFamilies == [
            "AF_INET"
            "AF_INET6"
            "AF_UNIX"
          ]
        )
        (hmService.UMask == "0077")
      ]
      {
        execStart = hmService.ExecStart;
        hardeningChecked = true;
      };
}
