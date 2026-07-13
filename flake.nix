{
  description = "Development environment for herdr-remote";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "aarch64-darwin"
        "aarch64-linux"
        "x86_64-darwin"
        "x86_64-linux"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      devShells = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          python = pkgs.python3.withPackages (ps: [
            ps.websockets
            ps.zeroconf
          ]);
        in
        {
          default = pkgs.mkShell {
            packages = [
              pkgs.gnumake
              pkgs.nixfmt
              pkgs.nodejs
              python
              pkgs.ruff
            ];
          };
        }
      );

      checks = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          python = pkgs.python3.withPackages (ps: [
            ps.websockets
            ps.zeroconf
          ]);
        in
        {
          relay =
            pkgs.runCommand "herdr-remote-check"
              {
                nativeBuildInputs = [
                  pkgs.gnumake
                  pkgs.nodejs
                  python
                  pkgs.ruff
                ];
              }
              ''
                cp -R ${self} source
                chmod -R u+w source
                cd source
                make check
                touch $out
              '';
        }
      );

      formatter = forAllSystems (system: nixpkgs.legacyPackages.${system}.nixfmt);
    };
}
