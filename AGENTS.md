# paddock

paddock runs one command in a sandbox (an Apple `container` VM, a rootful podman container, or a
bubblewrap namespace) with a resolved set of mounts, limits, and network policy.

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
  `paddock-init`, and the host watchdog, which `main` hands to `applecontainer.Dispatch` and
  `podman.Dispatch` before any other argument handling. Without those calls Run cannot start its
  watchdog, so every run fails.
- `internal/config`: strict TOML types and loaders for profiles and specs, config root
- `internal/resolve`: extends chain, trust rules, value sources, spec selection, mounts
- `internal/plan`: the Plan struct produced by resolve and consumed by engines, and the mount rule
  (`Mount.Check`) both apply
- `internal/initcfg`: the init config the host writes and paddock-init reads, and everything both
  sides share: the agent's home layout (`PrepareHome`, `HomeFS`), agent directories, agent env
  (`AgentEnv`), and the network policy names
- `internal/egresshost`: the egress host validator shared by config, resolve, and the guest proxy
- `internal/engine`: the Engine interface between a plan and the runtime that runs it
- `internal/engine/applecontainer`: the Apple `container` engine: image chain, mounts, run argv,
  init config, per-run directories
- `internal/engine/podman`: the rootful podman engine; the same shape over the `podman` CLI, with
  `--security-opt label=disable` for SELinux hosts and `Sudo` for rootful access
- `internal/engine/bwrap`: the bubblewrap engine, tier-0 same-user isolation: no image, no
  paddock-init, host or none egress only
- `internal/runstage`: the host-side run staging both engines share: run directories, mounts,
  secrets, the init config build, transcript paths, and the signal queue; nothing here may be
  imported by paddock-init
- `internal/imgtag`: the content-addressed image tag hash for the spec chain
- `images`: the embedded base image Dockerfile and the Claude Code version it installs
- `internal/guest/...`: in-guest enforcement (netpolicy, sniproxy)
- `internal/guest/pinit`: paddock-init, the guest entrypoint (`Main`): the agent and proxy
  accounts, the materialized agent HOME and `~/.claude` (via `initcfg.PrepareHome`), the agent
  environment and secrets, `/etc/hosts`, the network commit, and the privilege drop and exec of
  the agent; also the proxy child and the verify role

## Add an engine

The engines share their machinery in `runstage` and `initcfg`. Start a new engine from those
pieces; do not copy an existing engine's `Run`. Copied blocks drift, and every drift becomes a
divergence the engines disagree on. Reuse by default:

- Staging: `runstage.Stage` builds the passthrough environment, the init config, the managed
  settings write, and the seed files. It also creates the transcript directory 0700 — it holds
  agent sessions. Call it; do not re-stage.
- Environment: `initcfg.AgentEnv` sets HOME, USER, LOGNAME, SHELL, and PATH, then merges the
  config's env and secrets; `Stage` builds the passthrough lines it merges. Never read the
  invoking environment outside the engine's injectable hook.
- Home layout: `initcfg.PrepareHome` with a `HomeFS` materializes the agent HOME. The config's
  `Claude.TranscriptDir` decides what `~/.claude/projects` resolves through. Set it at the call
  site. Container engines set the writable transcript dir itself; pinit adds it to the guest's
  writable list. Bwrap sets the symlink target's parent, so the per-project dir under it is the
  bound transcript dir.
- Secrets: `initcfg.ParseSecrets` is the one parser for the secrets.env format. Its errors name
  the line and key, never the value. `runstage.ExitCode` is the one wait-error-to-exit-code rule.
- Mount rule: `runstage.CheckMountTargets` guards plan mounts against the engine's own guest
  paths. Add the engine's owned roots to its list rather than writing a second guard.

Every guard the engine enforces gets a test that fails when the guard is deleted. Pin observable
behavior: where files land on disk, not what a symlink's target string is. Every trust rule
applies to new code: secret values never reach errors, agent-written files are not world-readable,
and the paddock-init binary never imports `internal/runstage` (its test files may, to build init
configs). Own the child process's
lifecycle: every goroutine joins before `Run` returns, and the child dies with the parent
(watchdog or parent-death signal). Vet and build for every CI platform
(`GOOS=darwin go vet ./...`) before pushing. In tests, resolve both sides of any path
comparison — macOS resolves `/var` to `/private/var`.

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
- `make verify` runs fmtcheck, vet, test, build, build-init, and check. `make check` runs
  `cli-contract check` against the built binary, reading the surface `paddock schema`
  prints; CI runs it on both platforms.
- `make container-test` runs the engine tests that start real Apple `container` VMs and build
  images; `TestContainerRealInit` boots paddock-init in the base image. CI cannot run them: GitHub's
  macOS runners have no nested virtualization. A change to `internal/engine/applecontainer`, the run
  loop, or `internal/guest/pinit` needs a passing `make container-test` on Apple silicon before
  merge.
- `make container-test` also covers the podman engine when run on a Linux host with rootful podman
  (`TestPodmanRealInit`, gated on `PADDOCK_PODMAN_TESTS=1`; set `PADDOCK_PODMAN_SUDO=1` when podman
  is reached through sudo). The same change classes need a passing run there before merging a
  podman-side change.
- The bwrap engine's real test runs on any Linux host with bubblewrap:
  `PADDOCK_BWRAP_TESTS=1 go test ./internal/engine/bwrap -run TestBwrapReal`. A change to the bwrap
  engine or the agent-state staging (`runstage`, `paddock-init` home layout) needs a passing run
  before merging.
