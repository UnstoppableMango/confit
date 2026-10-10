{
  buildGoApplication,
  lib,
  git,
  version,
}:
buildGoApplication {
  pname = "confit";
  inherit version;

  src = lib.cleanSource ../.;
  modules = ./gomod2nix.toml;

  # The tests drive real git (>= 2.42) and build the confit binary for the
  # merge driver. The dconf tests are opt-in and stay off here.
  nativeCheckInputs = [ git ];
  preCheck = ''
    export HOME=$TMPDIR
  '';

  meta = {
    description = "Use git as a buffer and ledger between config UIs and the systems they configure";
    homepage = "https://github.com/UnstoppableMango/confit";
    license = lib.licenses.mit;
    mainProgram = "confit";
  };
}
