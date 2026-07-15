{ self }:
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.herdr-connector;
  stateDirectory = "herdr-connector";
  stateDir = "/var/lib/${stateDirectory}";
  certificateFile = "${stateDir}/client.crt";
  safeWssUrl = lib.types.strMatching "wss://[^/@?#[:space:]]+(/[^?#[:space:]]*)?";
  safeHttpsUrl = lib.types.strMatching "https://[^/@?#[:space:]]+(/[^?#[:space:]]*)?";
  uuid = lib.types.strMatching "[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}";
  displayName = lib.types.strMatching ".{1,80}";
  semanticVersion = lib.types.strMatching "[0-9]+\\.[0-9]+\\.[0-9]+([-+][A-Za-z0-9.-]+)?";
  instanceType = lib.types.submodule {
    options = {
      id = lib.mkOption {
        type = lib.types.strMatching "[A-Za-z0-9._-]{1,80}";
        description = "Herdr instance ID.";
      };
      socket = lib.mkOption {
        type = lib.types.strMatching "/.*";
        description = "Absolute Herdr Unix socket path.";
      };
    };
  };
in
{
  options.services.herdr-connector = {
    enable = lib.mkEnableOption "the Herdr Remote outbound connector";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.herdr-connector;
      defaultText = lib.literalExpression "herdr-remote.packages.\${pkgs.system}.herdr-connector";
      description = "Connector package to run.";
    };

    user = lib.mkOption {
      type = lib.types.str;
      description = "Existing system user that runs the Herdr server.";
    };

    group = lib.mkOption {
      type = lib.types.str;
      default = cfg.user;
      defaultText = lib.literalExpression "config.services.herdr-connector.user";
      description = "Existing system group used by the Herdr server.";
    };

    controlPlaneUrl = lib.mkOption {
      type = safeWssUrl;
      description = "Connector WSS endpoint without query credentials.";
    };

    hostId = lib.mkOption {
      type = uuid;
      description = "Host UUID assigned during enrollment.";
    };

    displayName = lib.mkOption {
      type = displayName;
      description = "Operator-facing host label.";
    };

    version = lib.mkOption {
      type = semanticVersion;
      default = "1.0.0";
      description = "Connector version reported during the protocol handshake.";
    };

    rotateUrl = lib.mkOption {
      type = lib.types.nullOr safeHttpsUrl;
      default = null;
      description = "Optional HTTPS mTLS certificate-rotation endpoint.";
    };

    instances = lib.mkOption {
      type = lib.types.listOf instanceType;
      description = "One to sixteen local Herdr instances.";
    };

    credentials = {
      initialCertFile = lib.mkOption {
        type = lib.types.strMatching "/.*";
        description = "Read-only initially enrolled client certificate copied into writable state on first start.";
      };
      keyFile = lib.mkOption {
        type = lib.types.strMatching "/.*";
        description = "Connector private key file.";
      };
      serverCaFile = lib.mkOption {
        type = lib.types.strMatching "/.*";
        description = "CA file used to verify the control-plane connector endpoint.";
      };
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.instances != [ ] && lib.length cfg.instances <= 16;
        message = "services.herdr-connector.instances must contain one to sixteen entries";
      }
      {
        assertion =
          lib.length (lib.unique (map (instance: instance.id) cfg.instances)) == lib.length cfg.instances;
        message = "services.herdr-connector.instances IDs must be unique";
      }
      {
        assertion = lib.trim cfg.displayName != "";
        message = "services.herdr-connector.displayName must not be only whitespace";
      }
      {
        assertion = lib.all (path: path != stateDir && !lib.hasPrefix "${stateDir}/" path) [
          cfg.credentials.initialCertFile
          cfg.credentials.keyFile
          cfg.credentials.serverCaFile
        ];
        message = "services.herdr-connector credential inputs must remain outside the writable state directory";
      }
    ];

    systemd.services.herdr-connector = {
      description = "Herdr Remote outbound connector";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];

      serviceConfig = {
        ExecStart = lib.escapeShellArgs (
          [
            "${cfg.package}/bin/herdr-connector"
            "-control-plane-url"
            cfg.controlPlaneUrl
            "-host-id"
            cfg.hostId
            "-display-name"
            cfg.displayName
            "-version"
            cfg.version
            "-cert-file"
            certificateFile
            "-initial-cert-file"
            (toString cfg.credentials.initialCertFile)
            "-key-file"
            (toString cfg.credentials.keyFile)
            "-server-ca-file"
            (toString cfg.credentials.serverCaFile)
          ]
          ++ lib.optionals (cfg.rotateUrl != null) [
            "-rotate-url"
            cfg.rotateUrl
          ]
          ++ lib.concatMap (instance: [
            "-herdr-instance"
            "${instance.id}=${instance.socket}"
          ]) cfg.instances
        );
        User = cfg.user;
        Group = cfg.group;
        StateDirectory = stateDirectory;
        StateDirectoryMode = "0700";
        ReadWritePaths = [ stateDir ];
        ReadOnlyPaths = [
          cfg.credentials.initialCertFile
          cfg.credentials.keyFile
          cfg.credentials.serverCaFile
        ];
        UMask = "0077";

        Restart = "on-failure";
        RestartSec = 10;
        TimeoutStopSec = 10;

        AmbientCapabilities = "";
        CapabilityBoundingSet = "";
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        NoNewPrivileges = true;
        PrivateDevices = true;
        PrivateTmp = true;
        ProtectClock = true;
        ProtectControlGroups = true;
        ProtectHome = "read-only";
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
