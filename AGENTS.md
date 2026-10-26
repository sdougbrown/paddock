# paddock

paddock runs one command in a sandbox (Apple `container` VM in v1) with a resolved set of mounts,
limits, and network policy.

## Layering rule

paddock decides mounts, limits, and network for one command; it never decides anything about agents.
Agent-specific behavior (what to run, which prompt, which skills) belongs to the caller.

## Trust rules

- Everything under the config root (`$PADDOCK_CONFIG_DIR`, else `$XDG_CONFIG_HOME/paddock`, else
  `~/.config/paddock`) is operator-owned (trust = operator). Repository-committed specs are
  trust = repository; the resolver enforces the rules for them even though v1 does not load them.
- No spec, at any trust level, may add a rw mount, set an env credential (`file:` or `env:` value
  source), or define host hooks. Profiles may not define host hooks either.
- The one rw mount a spec can cause is the setup-phase dependency cache: `setup.cache_dir` must lie
  under `/var/cache/paddock/`, and paddock picks the host directory under `~/.cache/paddock`. The
  run phase never mounts it. SECURITY.md residual risk 7 covers what it shares between workspaces.
- Only operator specs may define `[egress]` sets; a repository spec must extend an operator parent.
- A child spec adds contents and never overrides a parent (egress set, env name, setup command,
  managed-settings scalar).
- The profile decides access; the spec chain decides contents.
- Unknown TOML keys are errors. Errors name the file and key. Secret values never appear in the
  Plan, logs, or errors; value sources are resolved only at launch.

## Package layout

- `cmd/paddock`: thin CLI. It also has two re-exec roles: the guest `paddock-init` when argv[0] is
  `paddock-init`, and the host watchdog, which `main` hands to `applecontainer.Dispatch` before any
  other argument handling. Without that call Run cannot start its watchdog, so every run fails.
- `internal/config`: strict TOML types and loaders for profiles and specs, config root
- `internal/resolve`: extends chain, trust rules, value sources, spec selection, mounts
- `internal/plan`: the Plan struct produced by resolve and consumed by engines, and the mount rule
  (`Mount.Check`) both apply
- `internal/initcfg`: the init config the host writes and paddock-init reads, the guest paths both
  sides share, and the network policy names
- `internal/egresshost`: the egress host validator shared by config, resolve, and the guest proxy
- `internal/engine`: the Engine interface between a plan and the runtime that runs it
- `internal/engine/applecontainer`: the Apple `container` engine: image chain, mounts, run argv,
  init config, per-run directories
- `images`: the embedded base image Dockerfile and the Claude Code version it installs
- `internal/guest/...`: in-guest enforcement (netpolicy, sniproxy)
- `internal/guest/pinit`: paddock-init, the guest entrypoint (`Main`): the agent and proxy
  accounts, the agent's HOME and `~/.claude`, the agent environment and secrets, `/etc/hosts`, the
  network commit, and the privilege drop and exec of the agent; also the proxy child and the verify
  role

## Notes

- `SECURITY.md` holds the threat model and documented residual risks; update it with any change to them.
- The proxy denial log is diagnostic, not tamper-evident: any guest uid can write a rw virtiofs mount.
- Under the allowlist policy only the proxy uid may send DNS, so a lookup cannot carry data out.
  paddock-init maps each allowlisted host to `netpolicy.HostsAddr` (TEST-NET-1) in a root-owned
  `/etc/hosts`; tcp/443 to it is DNAT'd to the proxy, which resolves the SNI name itself through the
  pinned resolver and never reads `/etc/hosts`. Tools that bypass the hosts file (c-ares
  `dns.resolve*`, `dig`) get no answers in the guest.
- Egress hosts are exact names only in v1: no `*.domain` wildcards, IP literals, or `.invalid` and
  `.localhost` names. `internal/egresshost` is the one validator: spec loading, the chain merge, and
  the guest allowlist all call it, so paddock-init fails closed on a bad host in an init config.
  `/etc/hosts` cannot express wildcards, and they would let the agent make the proxy look up names
  of its choosing. List each host exactly.
- `make verify` runs fmtcheck, vet, test, build, and build-init.
- `make container-test` runs the engine tests that start real Apple `container` VMs and build
  images. CI cannot run them: GitHub's macOS runners have no nested virtualization. A change to
  `internal/engine/applecontainer` or the run loop needs a passing `make container-test` on Apple
  silicon before merge.
