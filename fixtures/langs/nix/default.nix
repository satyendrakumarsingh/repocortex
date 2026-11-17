{ pkgs ? import <nixpkgs> { } }:
pkgs.callPackage ./lib.nix {
  extra = 1;
}
