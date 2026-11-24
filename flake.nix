{
  description = "emuctl: Android emulators for people and AI agents, with a lease queue and an MCP server";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      # nixpkgs 26.11 dropped x86_64-darwin. Intel Macs can build with plain
      # `go build ./cmd/emuctl` (no cgo, no other dependencies).
      systems = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" ];
      forAll = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      version = "0.4.0";
    in {
      packages = forAll (pkgs: rec {
        emuctl = pkgs.buildGoModule {
          pname = "emuctl";
          inherit version;
          src = nixpkgs.lib.cleanSource ./.;
          vendorHash = null; # standard library only
          subPackages = [ "cmd/emuctl" ];
          env.CGO_ENABLED = "0";
          ldflags = [ "-s" "-w" "-X main.version=${version}" ];
          meta = {
            description = "Android emulators for people and AI agents: lease queue, MCP server, holder-only adb";
            homepage = "https://github.com/trbutler4/emuctl";
            mainProgram = "emuctl";
            platforms = nixpkgs.lib.platforms.linux ++ nixpkgs.lib.platforms.darwin;
          };
        };
        default = emuctl;
      });

      apps = forAll (pkgs: {
        default = { type = "app"; program = "${self.packages.${pkgs.stdenv.hostPlatform.system}.emuctl}/bin/emuctl"; };
      });

      # buildGoModule runs `go test ./...` in its check phase.
      checks = forAll (pkgs: { emuctl = self.packages.${pkgs.stdenv.hostPlatform.system}.emuctl; });

      devShells = forAll (pkgs: {
        default = pkgs.mkShell { packages = [ pkgs.go pkgs.gopls pkgs.gotools ]; };
      });

      overlays.default = final: _prev: {
        emuctl = self.packages.${final.stdenv.hostPlatform.system}.emuctl;
      };
    };
}
