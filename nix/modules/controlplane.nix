{ self }:
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.herdr-controlplane;
  stateDirectory = "herdr-controlplane";
  stateDir = "/var/lib/${stateDirectory}";
  vapidEnabled = cfg.vapid.publicKey != "";
  safeHttpsUrl = lib.types.strMatching "https://[^/@?#[:space:]]+(/[^?#[:space:]]*)?";
  boundedIdentity = lib.types.strMatching ".{1,256}";
in
{
  options.services.herdr-controlplane = {
    enable = lib.mkEnableOption "the Herdr Remote control plane";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.herdr-controlplane;
      defaultText = lib.literalExpression "herdr-remote.packages.\${pkgs.system}.herdr-controlplane";
      description = "Control-plane package to run.";
    };

    pwaPackage = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.herdr-pwa;
      defaultText = lib.literalExpression "herdr-remote.packages.\${pkgs.system}.herdr-pwa";
      description = "Built PWA assets served by the control plane.";
    };

    user = lib.mkOption {
      type = lib.types.str;
      default = "herdr-controlplane";
      description = "Dedicated system user for the control plane.";
    };

    group = lib.mkOption {
      type = lib.types.str;
      default = "herdr-controlplane";
      description = "Dedicated system group for the control plane.";
    };

    browserListen = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1:8080";
      description = "Browser HTTP listener behind the trusted OIDC reverse proxy.";
    };

    connectorListen = lib.mkOption {
      type = lib.types.str;
      default = ":8443";
      description = "Dedicated connector mTLS listener.";
    };

    origin = lib.mkOption {
      type = lib.types.strMatching "https://[^/@?#[:space:]]+";
      example = "https://herdr.example.com";
      description = "Exact HTTPS origin accepted for browser requests.";
    };

    database = lib.mkOption {
      type = lib.types.strMatching "/var/lib/herdr-controlplane/[A-Za-z0-9][A-Za-z0-9._-]*";
      default = "${stateDir}/control.db";
      description = "SQLite database path directly inside the service state directory.";
    };

    trustedProxyCIDRs = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [
        "127.0.0.0/8"
        "::1/128"
      ];
      description = "CIDRs from which trusted OIDC identity headers are accepted.";
    };

    oidc = {
      issuer = lib.mkOption {
        type = safeHttpsUrl;
        description = "Exact trusted OIDC issuer header value.";
      };
      audience = lib.mkOption {
        type = boundedIdentity;
        description = "Exact trusted OIDC audience header value.";
      };
      subject = lib.mkOption {
        type = boundedIdentity;
        description = "Single allowed OIDC subject.";
      };
      mfa = lib.mkOption {
        type = boundedIdentity;
        description = "Required MFA assurance header value.";
      };
    };

    credentials = {
      sessionSecretFile = lib.mkOption {
        type = lib.types.strMatching "/.*";
        description = "File containing the session secret.";
      };
      privateCaCertFile = lib.mkOption {
        type = lib.types.strMatching "/.*";
        description = "Connector issuing CA certificate file.";
      };
      privateCaKeyFile = lib.mkOption {
        type = lib.types.strMatching "/.*";
        description = "Connector issuing CA private key file.";
      };
      connectorTlsCertFile = lib.mkOption {
        type = lib.types.strMatching "/.*";
        description = "Connector listener TLS certificate file.";
      };
      connectorTlsKeyFile = lib.mkOption {
        type = lib.types.strMatching "/.*";
        description = "Connector listener TLS private key file.";
      };
      connectorClientCaFile = lib.mkOption {
        type = lib.types.strMatching "/.*";
        description = "CA file used to verify connector client certificates.";
      };
    };

    vapid = {
      publicKey = lib.mkOption {
        type = lib.types.str;
        default = "";
        description = "VAPID public key. Set all VAPID options to enable Web Push.";
      };
      privateKeyFile = lib.mkOption {
        type = lib.types.nullOr (lib.types.strMatching "/.*");
        default = null;
        description = "File containing the VAPID private key.";
      };
      subscriber = lib.mkOption {
        type = lib.types.str;
        default = "";
        example = "mailto:ops@example.com";
        description = "VAPID subscriber contact URI.";
      };
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = vapidEnabled == (cfg.vapid.privateKeyFile != null && cfg.vapid.subscriber != "");
        message = "services.herdr-controlplane.vapid options must be set together";
      }
    ];

    users.groups.${cfg.group} = { };
    users.users.${cfg.user} = {
      isSystemUser = true;
      group = cfg.group;
      home = stateDir;
    };

    systemd.services.herdr-controlplane = {
      description = "Herdr Remote control plane";
      wantedBy = [ "multi-user.target" ];
      after = [ "network.target" ];

      serviceConfig = {
        ExecStart = lib.escapeShellArgs (
          [
            "${cfg.package}/bin/herdr-controlplane"
            "-browser-listen"
            cfg.browserListen
            "-connector-listen"
            cfg.connectorListen
            "-origin"
            cfg.origin
            "-database"
            cfg.database
            "-static-dir"
            (toString cfg.pwaPackage)
            "-session-secret-file"
            (toString cfg.credentials.sessionSecretFile)
            "-private-ca-cert-file"
            (toString cfg.credentials.privateCaCertFile)
            "-private-ca-key-file"
            (toString cfg.credentials.privateCaKeyFile)
            "-connector-tls-cert-file"
            (toString cfg.credentials.connectorTlsCertFile)
            "-connector-tls-key-file"
            (toString cfg.credentials.connectorTlsKeyFile)
            "-connector-client-ca-file"
            (toString cfg.credentials.connectorClientCaFile)
            "-trusted-proxy-cidrs"
            (lib.concatStringsSep "," cfg.trustedProxyCIDRs)
            "-oidc-issuer"
            cfg.oidc.issuer
            "-oidc-audience"
            cfg.oidc.audience
            "-oidc-subject"
            cfg.oidc.subject
            "-oidc-mfa"
            cfg.oidc.mfa
          ]
          ++ lib.optionals vapidEnabled [
            "-vapid-public-key"
            cfg.vapid.publicKey
            "-vapid-private-key-file"
            (toString cfg.vapid.privateKeyFile)
            "-vapid-subscriber"
            cfg.vapid.subscriber
          ]
        );
        User = cfg.user;
        Group = cfg.group;
        StateDirectory = stateDirectory;
        WorkingDirectory = stateDir;
        UMask = "0077";

        Restart = "on-failure";
        RestartSec = 5;
        TimeoutStopSec = 15;

        AmbientCapabilities = "";
        CapabilityBoundingSet = "";
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        NoNewPrivileges = true;
        PrivateDevices = true;
        PrivateTmp = true;
        ProtectClock = true;
        ProtectControlGroups = true;
        ProtectHome = true;
        ProtectHostname = true;
        ProtectKernelLogs = true;
        ProtectKernelModules = true;
        ProtectKernelTunables = true;
        ProtectProc = "invisible";
        ProtectSystem = "strict";
        RestrictAddressFamilies = [
          "AF_INET"
          "AF_INET6"
          "AF_UNIX"
        ];
        RestrictNamespaces = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        SystemCallArchitectures = "native";
        SystemCallFilter = [
          "@system-service"
          "~@privileged"
        ];
      };
    };
  };
}
