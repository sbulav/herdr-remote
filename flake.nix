{
  description = "Herdr Remote control plane, outbound connector, and PWA";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    home-manager = {
      url = "github:nix-community/home-manager";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs =
    {
      self,
      nixpkgs,
      home-manager,
    }:
    let
      inherit (nixpkgs) lib;
      supportedSystems = [
        "aarch64-linux"
        "x86_64-linux"
      ];
      forAllSystems = lib.genAttrs supportedSystems;
      version = "1.0.0";
      goVendorHash = "sha256-+/zIUilpRLwMwAkl77/0TtX/bAxXx8ovwZsmvO31Xww=";
      npmDepsHash = "sha256-iXtYVVEa3Si3dh43gApjmZeRAEQN26JpywlRqoZ1Hkc=";

      buildHerdrGo =
        {
          pkgs,
          subPackage,
          pname,
          binary,
        }:
        pkgs.buildGoModule {
          inherit pname version;
          src = self;
          subPackages = [ subPackage ];
          vendorHash = goVendorHash;
          env.CGO_ENABLED = 0;
          ldflags = [
            "-s"
            "-w"
          ];
          postInstall = ''
            mv "$out/bin/${builtins.baseNameOf subPackage}" "$out/bin/${binary}"
          '';
          meta = {
            mainProgram = binary;
            license = lib.licenses.agpl3Only;
            platforms = supportedSystems;
          };
        };

      buildPwa =
        {
          pkgs,
          runTests ? false,
          npmDeps ? null,
        }:
        pkgs.buildNpmPackage (
          {
            pname = if runTests then "herdr-pwa-check" else "herdr-pwa";
            inherit version npmDepsHash;
            src = self;
            sourceRoot = "source/web";
            nodejs = pkgs.nodejs_22;
            npmBuildScript = if runTests then "verify:ci" else "build";
            installPhase = ''
              runHook preInstall
              mkdir -p "$out"
              if [ -d dist ]; then
                cp -r dist/. "$out/"
              else
                touch "$out/pwa-check-passed"
              fi
              runHook postInstall
            '';
            meta = {
              license = lib.licenses.agpl3Only;
              platforms = supportedSystems;
            };
          }
          // lib.optionalAttrs (npmDeps != null) { inherit npmDeps; }
        );

      perSystem =
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          runGoCheck =
            name: command: cgoEnabled: goModules:
            pkgs.runCommand name
              {
                nativeBuildInputs = [ pkgs.go ] ++ lib.optionals cgoEnabled [ pkgs.gcc ];
                src = self;
              }
              ''
                cp -R "$src" source
                chmod -R u+w source
                cd source
                export HOME="$TMPDIR"
                export GOCACHE="$TMPDIR/go-cache"
                cp -R "${goModules}" vendor
                chmod -R u+w vendor
                export GOFLAGS="-mod=vendor"
                export GOPROXY=off
                export CGO_ENABLED=${if cgoEnabled then "1" else "0"}
                ${command}
                touch "$out"
              '';
        in
        rec {
          packages = {
            herdr-controlplane = buildHerdrGo {
              inherit pkgs;
              subPackage = "cmd/controlplane";
              pname = "herdr-controlplane";
              binary = "herdr-controlplane";
            };
            herdr-connector = buildHerdrGo {
              inherit pkgs;
              subPackage = "cmd/connector";
              pname = "herdr-connector";
              binary = "herdr-connector";
            };
            herdr-pwa = buildPwa { inherit pkgs; };
            default = packages.herdr-controlplane;
          };

          checks = {
            inherit (packages) herdr-controlplane herdr-connector herdr-pwa;
            go-vet = runGoCheck "herdr-go-vet" "go vet ./..." false packages.herdr-controlplane.goModules;
            go-tests = runGoCheck "herdr-go-tests" "go test ./..." false packages.herdr-controlplane.goModules;
            go-race =
              runGoCheck "herdr-go-race" "go test -race ./..." true
                packages.herdr-controlplane.goModules;
            protocol-tests =
              pkgs.runCommand "herdr-protocol-tests"
                {
                  nativeBuildInputs = [ pkgs.python3 ];
                  src = self;
                }
                ''
                  cp -R "$src" source
                  chmod -R u+w source
                  cd source
                  python3 -m unittest discover -s tests -p 'test_*.py'
                  touch "$out"
                '';
            pwa-verify = buildPwa {
              inherit pkgs;
              runTests = true;
              npmDeps = packages.herdr-pwa.npmDeps;
            };
          }
          // import ./nix/tests/modules.nix {
            inherit
              self
              system
              pkgs
              nixpkgs
              home-manager
              ;
          };
        };
    in
    {
      packages = forAllSystems (system: (perSystem system).packages);
      checks = forAllSystems (system: (perSystem system).checks);

      devShells = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = pkgs.mkShell {
            packages = [
              pkgs.go
              pkgs.gnumake
              pkgs.nixfmt
              pkgs.nodejs_22
              pkgs.python3
            ];
          };
        }
      );

      nixosModules = {
        controlplane = import ./nix/modules/controlplane.nix { inherit self; };
        connector = import ./nix/modules/connector.nix { inherit self; };
      };

      homeManagerModules.connector = import ./nix/modules/connector-home.nix { inherit self; };

      formatter = forAllSystems (system: nixpkgs.legacyPackages.${system}.nixfmt);
    };
}
