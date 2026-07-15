{
  # This example assumes the flake's NixOS connector module is imported and
  # the existing `herdr` user runs the local Herdr server.
  services.herdr-connector = {
    enable = true;
    user = "herdr";
    controlPlaneUrl = "wss://connectors.herdr.example.com:8443/v1/connectors/ws";
    rotateUrl = "https://connectors.herdr.example.com:8443/v1/connectors/rotate";
    hostId = "REPLACE_WITH_ENROLLED_HOST_UUID";
    displayName = "workstation";

    credentials = {
      initialCertFile = "/run/secrets/herdr/client.crt";
      keyFile = "/run/secrets/herdr/client.key";
      serverCaFile = "/run/secrets/herdr/connector-server-ca.crt";
    };

    instances = [
      {
        id = "default";
        socket = "/home/herdr/.config/herdr/herdr.sock";
      }
    ];
  };
}
