# Source nixpkgs from the live nixos-unstable channel so legacy `nix-build`
# tracks the same fast-moving Go toolchain used by shell.nix and checks.nix.
# Reproducibility is the flake's job (flake.nix pins via flake.lock). Local
# development should always run against the latest unstable. Defaulting to
# `import <nixpkgs> {}` resolved to an older channel whose Go predates the
# go.mod floor, forcing a toolchain download in the build sandbox.
let
  unstable = builtins.fetchTarball {
    url = "https://github.com/NixOS/nixpkgs/archive/nixos-unstable.tar.gz";
  };
in

{ pkgs ? import unstable {} }:

pkgs.stdenv.mkDerivation rec {
  pname = "cex-dra-kubeletplugin";
  # Tracks the newest release heading in CHANGELOG.md, unprefixed per nix
  # convention. `make release-check` gates the pair staying in step.
  version = "1.0.0-alpha.1";

  src = ../../.;   # repo root (where go.mod and Makefile live)

  nativeBuildInputs = [ pkgs.go ];

  buildPhase = ''
    export HOME=$TMPDIR
    export GOCACHE=$TMPDIR/go-cache
    # The go.mod toolchain pin targets network builds (dev shell, image
    # build). The sandbox cannot download toolchains, so run nixpkgs go.
    # The go directive is the floor that still guards compatibility here.
    export GOTOOLCHAIN=local
    make
  '';

  installPhase = ''
    make install PREFIX=$out
  '';
}
