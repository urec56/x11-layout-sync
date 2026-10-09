{
  description = "Sub-millisecond keyboard layout sync between a host and an X11 guest VM";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";
  };

  outputs = { self, nixpkgs }:
    let
      supportedSystems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = nixpkgs.lib.genAttrs supportedSystems;
    in
    {
      packages = forAllSystems (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        rec {
          x11-layout-sync = pkgs.stdenv.mkDerivation {
            pname = "x11-layout-sync";
            version = "0.0.1";

            src = ./.;

            nativeBuildInputs = [ pkgs.go ];

            # Fully static binary: CGO_ENABLED=0 → no glibc dependency, runs
            # on any Linux (host and guest) without a matching nix/glibc.
            # Set in the build phase (not via buildGoModule, whose CGO_ENABLED
            # handling differs across nixpkgs versions).
            dontInstall = true;
            buildPhase = ''
              runHook preBuild
              export CGO_ENABLED=0
              export GO111MODULE=on
              export GOCACHE="$TMPDIR/go-cache"
              export GOPATH="$TMPDIR/go"
              mkdir -p "$out/bin"
              go build -ldflags "-s -w" -o "$out/bin/x11-layout-sync" .
              runHook postBuild
            '';

            meta = {
              description = "Sub-millisecond keyboard layout sync between a host and an X11 guest VM";
              license = pkgs.lib.licenses.mit;
              platforms = pkgs.lib.platforms.linux;
            };
          };

          default = x11-layout-sync;
        }
      );

      devShells = forAllSystems (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = pkgs.mkShell {
            buildInputs = [ pkgs.go ];
          };
        }
      );
    };
}
