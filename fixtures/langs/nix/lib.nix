let
  version = "1.2";
  double = x: x * 2;
  add = a: b: a + b;
  inherit (builtins) map;
  nested.deep = 1;
in
{
  name = "pkg-${version}";
  inherit version;
  build = { stdenv, fetchurl }: stdenv.mkDerivation {
    pname = "demo";
    src = fetchurl { url = "x"; };
  };
  helper = x: x + 1;
  counter = 0;
  settings = {
    enable = true;
    run = cmd: cmd;
  };
  "quoted-name" = 5;
  inherit double add;
}
