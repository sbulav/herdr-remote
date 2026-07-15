#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
repository_root="$(cd "$project_root/.." && pwd)"
nix_expression="let flake = builtins.getFlake \"$repository_root\"; pkgs = import flake.inputs.nixpkgs { system = builtins.currentSystem; }; in import $project_root/scripts/playwright-nixos.nix { inherit pkgs; }"
runtime="$(nix build --impure --no-link --print-out-paths --expr "$nix_expression")"

cd "$project_root"
exec "$runtime/bin/herdr-playwright-fhs" -lc 'bash scripts/run-playwright-in-fhs.sh'
