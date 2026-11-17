{ config, lib, pkgs, ... }:

let
  cfg = config.services.demo;
  mkThing = name: pkgs.writeText name "x";
in
{
  options.services.demo.enable = lib.mkEnableOption "demo";

  config = lib.mkIf cfg.enable {
    environment.systemPackages = [ pkgs.hello ];
  };
}
