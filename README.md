# paddock

Run one command in an Apple `container` VM with explicit mounts, limits, and a network policy.

To inspect a plan without starting a VM, run this from a checkout of paddock:

```sh
make build
mkdir -p /tmp/paddock-example/repo
PADDOCK_CONFIG_DIR="$PWD/internal/resolve/testdata/root" bin/paddock plan \
  --profile fixer --spec afresh --workspace /tmp/paddock-example -- claude
```

The fixture profile mounts the `repo` subdirectory read-only. The command prints JSON; it does not run Claude. For your own configuration, start with the [profile walkthrough](docs/profiles.md) and use the [configuration reference](docs/config.md) for all keys and defaults. Read [Security](SECURITY.md) for the threat model and residual risks.

## Install

Download the latest release archive and extract it into a directory on `PATH`:

```sh
gh release download --repo sdougbrown/paddock --pattern paddock_darwin_arm64.tar.gz
mkdir -p ~/.local/bin
tar -xzf paddock_darwin_arm64.tar.gz -C ~/.local/bin
```

The binaries are not notarized. `gh` and `curl` downloads carry no quarantine attribute; for an archive downloaded in a browser, run `xattr -d com.apple.quarantine ~/.local/bin/paddock` before the first run. The archive includes `paddock-init`, the Linux guest entrypoint. paddock looks for it beside its own executable; set `PADDOCK_INIT` to use another path.

## Commands

`plan`, `run`, and `setup` take `--profile <name>` (required), `--spec <ref>` (otherwise selected from Git origin or the profile default), and `--workspace <dir>` (default: current directory). Put flags before `--` and the command.

- `paddock plan [flags] [-- command ...]` prints the resolved plan as JSON. `--phase run|setup` defaults to `run`; a setup plan takes no command.
- `paddock run [flags] -- command ...` starts the command. `--verify` runs isolation checks instead and takes no command.
- `paddock setup [flags]` runs the spec's setup command; it takes no command argument and needs a writable workspace.
- `paddock version` prints the version.

Help is per command: use `paddock plan -h`, for example, or `-h` or `--help` on `run` or `setup`. Top-level `paddock --help` reports an unknown command. `plan` does not start a VM; `run` and `setup` do.

## Troubleshooting

- **Claude stops at “Managed settings require approval”.** If your organization pushes managed settings, list the host's `~/.claude/remote-settings.json` in the profile's `[claude] seed_files`.
- **Claude's own sandbox fails in the guest.** Set `sandbox.enabled` to `false` in Claude settings. Do not use `CLAUDE_CODE_SUBPROCESS_ENV_SCRUB`: it crashes Claude in the guest because it needs bubblewrap, which cannot mount `/proc` there.
- **`container system status` says “not running”.** It can report that while the service works. Check with `container list`.
- **A memory-heavy run stalls.** This is a known issue at the default memory limit. A run can stop producing output and use no CPU, and `container kill` and `container exec` may also hang. Send SIGTERM to the `paddock run` process, then SIGKILL if it does not exit. After paddock dies, its watchdog attempts to stop and delete the VM. A hung container service can prevent cleanup. Check `container list`. For a Node monorepo test run, try `memory = "4G"` in the run profile's `[limits]`; see [Networked setup, offline run](docs/profiles.md#networked-setup-offline-run).
- **An experimental config edit changed the live config.** If `~/.config/paddock` is a symlink, copy it with `cp -RL` for experiments. Otherwise edits through the symlink land in the live config. Point `PADDOCK_CONFIG_DIR` at the copy.
- **An image disappears during concurrent runs.** An image chain rebuild prunes the chain's other tags. A concurrent run can lose an image before it starts; retry it. See [Security](SECURITY.md) for the image-pruning residual risk.
