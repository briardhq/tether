{
  description = "briard-tether — a USB Zigbee coordinator, served over the network";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = f:
        nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      # The packaged binary, and the only build of it that is anybody's product: one static
      # binary, nothing else. There is no systemd unit or udev rule here to go with it, and that
      # is the design rather than an omission — the binary writes both itself
      # (`briard-tether install`), so a package cannot ship one without the other.
      packages = forAllSystems (pkgs: rec {
        default = briard-tether;
        briard-tether = let
          # The revision: a source build is not a release, and a semver here would claim to be
          # one. A dirty tree says so. The binary reports it through `briard-tether version`.
          version = self.shortRev or self.dirtyShortRev or "unknown";
        in pkgs.buildGoModule {
          pname = "briard-tether";
          inherit version;
          src = ./.;
          # Keyed on what the code IMPORTS, not on what go.mod lists: buildGoModule vendors
          # reached packages only, so this moves when a new import first reaches a module
          # go.sum already carried -- with go.mod and go.sum both untouched.
          vendorHash = "sha256-kBSIbly0lB4p0SiogsV8/TCgfxAyVuEHp0E5MY3n+4E=";

          # ⚠️ **Static, and explicitly so.** A plain `go build` does not produce one: Go leaves
          # cgo on wherever the host has a C compiler, so `net` pulls in `runtime/cgo` and the
          # result is dynamically linked against this machine's `ld-linux`, a path no target
          # has. The costs are the pure-Go DNS resolver and `os/user`, and
          # both are what this binary wants anyway — the install verb's group lookups read
          # /etc/group, which is where a tty group lives.
          env.CGO_ENABLED = 0;
          ldflags = [ "-X briard.io/tether/internal/build.Version=${version}" ];

          # The product, not the test tree: `tests/leash` is a Windows-only harness helper and
          # nothing here is meant to build it.
          subPackages = [ "cmd/briard-tether" ];

          # The suite is CI's job, and it wants a pty pair, a TCP listener and a few seconds —
          # none of which belong in a package build that a consumer will run on every bump.
          doCheck = false;

          meta = {
            description = "A USB Zigbee coordinator, served over the network";
            mainProgram = "briard-tether";
            platforms = pkgs.lib.platforms.linux ++ pkgs.lib.platforms.windows;
          };
        };
      });

      # The dev shell: `nix develop`, or `direnv allow` with the checked-in .envrc.
      devShells = forAllSystems (pkgs:
        let
          # Home Assistant with the components the ZHA gate needs, wrapped so that `bin/hass` can
          # actually import them — see the `zha` shell below for why both halves are here.
          withZha = pkgs.home-assistant.override {
            extraComponents = [ "zha" "frontend" "zeroconf" ];
          };
          hass = pkgs.runCommand "hass-with-zha" { nativeBuildInputs = [ pkgs.makeWrapper ]; } ''
            mkdir -p $out/bin
            makeWrapper ${withZha}/bin/hass $out/bin/hass \
              --prefix PYTHONPATH : "${withZha.pythonPath}"
          '';

          # Everything both shells need. `zha` below is this shell plus one very large
          # package, so the list lives here rather than being inherited: `inputsFrom` would
          # carry the packages and silently drop the two environment variables under it,
          # which is the sort of difference that is found at 2am in a failing harness.
          common = {
            packages = with pkgs; [
              go
              gopls
              gotools # goimports, etc.
              go-tools # staticcheck

              # The first client gate: a real zigpy client driving tether over socket://. Python
              # is here because the client is — test helpers may be written in the upstream tool's
              # language, and there is no Go Zigbee stack to prefer. Nothing in the shipped
              # binary knows this exists.
              #
              # From nixpkgs rather than pip, and that is not a preference: the PyPI sdist of
              # zigpy-znp ships no conftest.py and no tests/nvram, so pip cannot reach the
              # coordinator emulator the harnesses need. nixpkgs builds from the GitHub archive, which
              # carries the whole test tree, and flake.lock pins it.
              (python3.withPackages (ps: [
                ps.zigpy-znp
                # Their suite's own test requirements (requirements_test.txt), so that
                # tests/run_suite.py can run it unmodified. pytest-cov is left out: we want
                # their assertions, not their coverage report.
                ps.pytest
                ps.pytest-asyncio
                ps.pytest-mock
                ps.pytest-timeout
              ]))

              # The second client gate: zigbee-herdsman, the TypeScript stack behind
              # zigbee2mqtt. It is a second *implementation* rather than more of the same —
              # nothing proved about zigpy proves anything about it — and it is the only client
              # that speaks `mdns://`, the zero-config route the TXT contract exists to
              # serve. Same reasoning as the Python above: the client is TypeScript, so the
              # harness is.
              #
              # zigbee2mqtt is here for what it bundles, not to be run: nixpkgs installs its
              # `node_modules` complete, so `zigbee-herdsman` and its dependencies come out of
              # flake.lock with no npm install and nothing fetched at test time.
              nodejs
              zigbee2mqtt

              # The broker zigbee2mqtt insists on before it will start. Z2M has no
              # embedded option and no offline mode, so this is not a preference — without a broker
              # there is no Z2M to test. It also *replaces* a dependency rather than adding one:
              # `mosquitto_sub` reads `bridge/state` and `bridge/info` well enough that the harness
              # needs no Python MQTT client, and both topics are published retained, so a subscriber
              # started after Z2M still sees them. Nothing in the shipped binary speaks MQTT.
              mosquitto

              # For the one line of tests/run_gate.py that brings loopback up inside the network
              # namespace the mDNS gate runs in. Pinned rather than taken off the host's PATH for
              # the same reason as everything above it: the gate should behave the same on a
              # machine that is not this one.
              iproute2
            ];

            # Where `require("zigbee-herdsman")` finds it. zigbee2mqtt's node_modules is a pnpm
            # tree, so `zigbee-herdsman` here is a symlink into `.pnpm/zigbee-herdsman@<version>`
            # and node resolves its dependencies from the real path — which keeps the version out
            # of this string, so a nixpkgs bump does not have to be mirrored here.
            #
            # NODE_PATH rather than an env var of our own: it is node's own mechanism, nothing
            # else in this shell is a node program, and it means the harness needs no wrapper.
            NODE_PATH = "${pkgs.zigbee2mqtt}/lib/node_modules/zigbee2mqtt/node_modules";

            # zigpy-znp's coordinator emulator lives in its *test* tree, which no released
            # package ships — the PyPI sdist has no conftest.py and no tests/nvram. nixpkgs builds
            # from the GitHub archive, so its unpacked source has the whole suite, and this points
            # at it. pytest is in the shell for the same reason: their conftest imports it.
            ZIGPY_ZNP_SRC = pkgs.python3Packages.zigpy-znp.src;
            # Install the tracked git hooks on shell entry. Symlink (not copy) so edits to
            # scripts/hooks/ take effect without re-running install. Skip if .git is absent.
            shellHook = ''
              if [ -d .git/hooks ] && [ ! -L .git/hooks/pre-commit ]; then
                ln -sf ../../scripts/hooks/pre-commit .git/hooks/pre-commit
              fi
            '';
          };
        in
        {
          default = pkgs.mkShell common;

          # The ZHA client gate: Home Assistant with the `zha` integration, which is a
          # different thing from the `ControllerApplication` the first gate drives — the config flow, the
          # zeroconf card and the reload-on-disconnect live above that object and nowhere else.
          # Its own shell because of its size: 310 MiB to fetch and 1.3 GiB unpacked, against a
          # default shell that a one-line Go change should be able to enter in seconds. Nothing
          # is built locally — it all comes from the binary cache — so the cost is a download
          # once, paid by whoever runs this harness and by nobody else.
          # ⚠️ **An env var and not a package, and that is not a style choice.** Home Assistant
          # is built against python 3.14 here and this shell's own python is 3.13; putting it in
          # `packages` runs nixpkgs' python setup hook, which exports a PYTHONPATH of 3.14
          # site-packages into the shell — and the *first* thing our 3.13 imports from it dies
          # with `undefined symbol: PyType_Freeze` from somebody else's compiled cryptography.
          # `bin/hass` is a wrapper that sets its own PYTHONPATH, so pointing at it leaves both
          # interpreters intact. Same idiom as NODE_PATH and ZIGPY_ZNP_SRC above: a store path
          # the harness is handed, rather than a thing on PATH.
          # ⚠️ **Three components, and `frontend` is not there for a UI.** nixpkgs'
          # home-assistant ships *no* component dependencies unless they are named here, and a
          # Home Assistant whose `frontend` fails to import **falls back to recovery mode**,
          # where the config-entries API this harness drives does not exist at all. `frontend`
          # is what pulls in `api`, `auth`, `config`, `onboarding` and `websocket_api` as
          # dependencies, so naming it is how the REST surface exists. `zeroconf` is for the
          # discovery case alone. Nothing else: no `recorder`, no `default_config` — a database
          # and a bluetooth stack are not part of what this measures.
          # ⚠️ **And the components have to be wrapped in by hand.** nixpkgs' `extraComponents`
          # does not change the home-assistant derivation at all — it only computes
          # `passthru.pythonPath`, which the NixOS module puts on the wrapper. Off NixOS that
          # step is ours, and without it `$HASS` starts, fails to import `hass_frontend`, and
          # silently drops into **recovery mode**, where the API this harness drives is absent.
          zha = pkgs.mkShell (common // {
            HASS = "${hass}/bin/hass";
          });
        });
    };
}
