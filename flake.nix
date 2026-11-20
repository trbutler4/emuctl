{
  description = "emuctl: shared Android emulator pool with a lease queue";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAll = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in {
      packages = forAll (pkgs: rec {
        emuctl = pkgs.buildGoModule {
          pname = "emuctl";
          version = "0.1.0";
          src = ./.;
          vendorHash = null;
          subPackages = [ "cmd/emuctl" ];
          meta.mainProgram = "emuctl";
        };
        default = emuctl;
      });
      devShells = forAll (pkgs: { default = pkgs.mkShell { packages = [ pkgs.go pkgs.gopls ]; }; });
    };
}
