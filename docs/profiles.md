# Build a reviewer and a coder profile

Use separate profiles for a reviewer that only reads a repository and a coder that changes it. This walkthrough uses one operator-owned spec for both roles.

The reviewer can reach Anthropic in the run phase. The coder can also reach the npm registry during setup. The registry is not reachable during the run phase.

For all commands below, you need:

- paddock and Apple `container`, installed on an Apple silicon Mac.
- The container service, started.
- An existing Node workspace with a `package-lock.json`.

The lockfile must use packages available from `registry.npmjs.org`; additional hosts in a lockfile require explicit setup egress entries. Set these paths in your shell:

```sh
export PADDOCK_CONFIG_DIR="$HOME/paddock-example"
export WORKSPACE="$HOME/Code/my-node-repo"
mkdir -p "$PADDOCK_CONFIG_DIR/profiles" "$PADDOCK_CONFIG_DIR/projects" \
  "$PADDOCK_CONFIG_DIR/images/node" "$PADDOCK_CONFIG_DIR/claude"
```

Use an API credential available to Claude Code. For example, store an Anthropic API key in a private host file and protect it with `chmod 600 ~/.secrets/anthropic-key`. Do not put the key in TOML. The examples use `ANTHROPIC_API_KEY` from that file; use your organization's supported authentication method if different. Setup does not receive this credential.

## Shared project spec

Create `$PADDOCK_CONFIG_DIR/projects/node.toml`:

```toml
[image]
dockerfile = "images/node/Dockerfile"

[egress]
anthropic = ["api.anthropic.com"]
npm = ["registry.npmjs.org"]

[setup]
command = ["npm", "ci", "--cache", "/var/cache/paddock/npm"]
cache_key = "package-lock.json"
cache_dir = "/var/cache/paddock/npm"

[claude]
managed_settings = "claude/node-settings.json"

[env]
CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC = "1"
```

Create `$PADDOCK_CONFIG_DIR/images/node/Dockerfile`:

```dockerfile
ARG PADDOCK_PARENT
FROM ${PADDOCK_PARENT}
```

This minimal layer inherits the base image's Node and Claude Code. Add your project's tools to this Dockerfile if needed; image builds run on the host, outside the guest egress policy. Create `$PADDOCK_CONFIG_DIR/claude/node-settings.json`:

```json
{
  "sandbox": { "enabled": false }
}
```

The JSON disables Claude Code's own sandbox inside paddock. It does not change paddock's isolation.

The setup command writes `node_modules` into the workspace and uses the cache only during setup. npm lifecycle scripts also run with workspace write access. Use a trusted workspace, and review its contents before running them on the host. See [Security](../SECURITY.md).

## Read-only reviewer

Create `$PADDOCK_CONFIG_DIR/profiles/reviewer.toml`:

```toml
engine = "apple-container"
default_spec = "node"

[workspace]
mode = "ro"

[network]
policy = "allowlist"
run = ["anthropic"]
setup = ["anthropic", "npm"]

[env]
ANTHROPIC_API_KEY = "file:~/.secrets/anthropic-key"
```

Inspect the run plan without starting a VM:

```sh
paddock plan --profile reviewer --spec node --workspace "$WORKSPACE" -- claude
```

The JSON shows a read-only workspace, `api.anthropic.com` in `network.hosts`, and a `file` reference rather than the key. The `setup` object comes from the spec, but this profile cannot run it. Both commands fail before any VM starts:

```sh
paddock setup --profile reviewer --spec node --workspace "$WORKSPACE"
# paddock setup: phase setup: setup needs a read-write workspace; profile "reviewer" workspace.mode is ro
paddock plan --profile reviewer --spec node --workspace "$WORKSPACE" --phase setup
# paddock plan: phase setup: setup needs a read-write workspace; profile "reviewer" workspace.mode is ro
```

Run setup with the coder profile below if the reviewer needs dependencies in the workspace. Verify the reviewer boundary, then launch Claude:

```sh
paddock run --profile reviewer --spec node --workspace "$WORKSPACE" --verify
paddock run --profile reviewer --spec node --workspace "$WORKSPACE" -- claude
```

## Read-write coder

Create `$PADDOCK_CONFIG_DIR/profiles/coder.toml`:

```toml
engine = "apple-container"
default_spec = "node"

[workspace]
mode = "rw"

[network]
policy = "allowlist"
run = ["anthropic"]
setup = ["anthropic", "npm"]

[env]
ANTHROPIC_API_KEY = "file:~/.secrets/anthropic-key"
```

Run these commands from outside the workspace or from its root so `package-lock.json` resolves to the workspace's root lockfile:

```sh
paddock plan --profile coder --spec node --workspace "$WORKSPACE" -- claude
paddock plan --profile coder --spec node --workspace "$WORKSPACE" --phase setup
paddock setup --profile coder --spec node --workspace "$WORKSPACE"
paddock run --profile coder --spec node --workspace "$WORKSPACE" --verify
paddock run --profile coder --spec node --workspace "$WORKSPACE" -- claude
```

The setup plan lists both egress sets and the cache mount. The run plan lists only Anthropic and no cache mount. Setup prints one summary line to stderr, such as `paddock setup: cache miss, <cache dir>, <duration>, exit 0`. A hit reports a previous successful setup; it does not skip `npm ci`.

Setup can fail if the lockfile references another host or an install script needs other network access. Review the lockfile and add only required hosts to the spec's setup set.

`--verify` runs checks instead of a command. Lines have the form `PASS <check>: <detail>` or `FAIL <check>: <detail>`. It prints no summary line. The number of checks depends on the policy and mounts.

Checks include mount access, dropped privileges, and the configured network boundary. An allowlisted host must be reachable for its connectivity check to pass. A failure gives a nonzero exit status; investigate before using the profile. A successful check is not a guarantee against every escape or data leak; see [Security](../SECURITY.md).

To launch a different agent, put its executable in the image or an accessible mount and pass its arguments after `--`:

```sh
paddock run --profile coder --spec node --workspace "$WORKSPACE" -- opencode acp
```

The base image does not install OpenCode. Install it in your image layer before using this command, and configure any credentials and egress it needs. The example spec permits only Anthropic during run.

## Networked setup, offline run

Use two profiles with the same spec:

- The setup profile uses `network.policy = "allowlist"` and names the required egress sets in `network.setup`. It needs `network.run` sets only if you also run a command with that profile.
- The run profile uses `network.policy = "none"`.

Setup needs a writable workspace. The cache mount is available only during setup, so install dependencies into the workspace for the offline run. Put tools in the image, not the setup cache.

Limits belong to each profile, not the spec. Set `[limits]` in each profile that needs them.

By default, `limits.cpus` is `0` and `limits.memory` is empty; paddock passes neither limit to Apple `container`, which chooses its defaults. A cold test observed 1024 MB of memory. For a Node monorepo test run, start with `memory = "4G"` in the run profile:

```toml
[limits]
memory = "4G"
```

For a known issue with stalled memory-heavy runs and recovery, see [Troubleshooting](../README.md#troubleshooting). The following Go and Yarn examples use this pattern.

## Offline tests for a Go module

Setup vendors dependencies into the workspace; the setup cache itself is not mounted during run. Use the [networked setup, offline run](#networked-setup-offline-run) pattern above.

Choose a Go module with a `go.sum` file, and run the commands from its root. This example uses `owner/repo` as the spec reference; replace both components with names for your project. Create the directories under your config root:

```sh
mkdir -p "$PADDOCK_CONFIG_DIR/projects/owner" \
  "$PADDOCK_CONFIG_DIR/profiles" "$PADDOCK_CONFIG_DIR/images/go"
```

Create `$PADDOCK_CONFIG_DIR/projects/owner/repo.toml`:

```toml
[image]
dockerfile = "images/go/Dockerfile"

[egress]
gomod = ["proxy.golang.org", "sum.golang.org"]

[setup]
command = ["sh", "-c", "GOMODCACHE=/var/cache/paddock/gomod go mod vendor"]
cache_key = "go.sum"
cache_dir = "/var/cache/paddock/gomod"

[env]
GOFLAGS = "-mod=vendor"
```

Create `$PADDOCK_CONFIG_DIR/images/go/Dockerfile`, using a Go version suitable for your module. This example uses the version in paddock's `go.mod`:

```dockerfile
ARG PADDOCK_PARENT
FROM golang:1.26 AS gotool
FROM ${PADDOCK_PARENT}
COPY --from=gotool /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"
```

Create `$PADDOCK_CONFIG_DIR/profiles/go-tester.toml` for the offline run:

```toml
engine = "apple-container"
default_spec = "owner/repo"

[workspace]
mode = "rw"

[network]
policy = "none"
```

Create `$PADDOCK_CONFIG_DIR/profiles/go-tester-setup.toml` for setup:

```toml
engine = "apple-container"
default_spec = "owner/repo"

[workspace]
mode = "rw"

[network]
policy = "allowlist"
setup = ["gomod"]
```

Pass the spec explicitly to avoid selection from the workspace's Git origin:

```sh
paddock setup --profile go-tester-setup --spec owner/repo
paddock run --profile go-tester --spec owner/repo --verify
paddock run --profile go-tester --spec owner/repo -- go test ./...
```

Setup writes `vendor/` into the workspace. `GOFLAGS` makes the run use it rather than the unmounted module cache. A first setup with no completed cache reports a miss. A later successful setup with the same spec and `go.sum` reports a hit and still runs `go mod vendor`.

The verify command prints `PASS` or `FAIL` for each check. The test command prints Go's package results. The number of verify checks depends on the policy and mounts.

A cold test of this pattern reported a cache miss then a hit. Verify had 16 passes and no failures, and the Go tests passed offline.

See [Setup cache](config.md#setup-cache) for cache cleanup and risks, and [Verify checks](config.md#verify-checks) for checks by policy.

## Yarn and pnpm

Use the [networked setup, offline run](#networked-setup-offline-run) pattern for a Yarn or pnpm project. Dependencies go in the workspace during setup; package-manager binaries go in the image. Corepack downloads a missing Yarn binary from `repo.yarnpkg.com`, but an offline run has no network or setup cache mount. Bake the required version into a spec image layer instead. For Yarn 4.13.0:

```dockerfile
ARG PADDOCK_PARENT
FROM ${PADDOCK_PARENT}
ENV COREPACK_HOME=/usr/local/share/corepack
RUN corepack prepare yarn@4.13.0 --activate && chmod -R a+rX /usr/local/share/corepack
```

Image builds run on the host with network access, outside the guest egress policy. Do not put `COREPACK_HOME` in the workspace. In a package with `"type": "module"`, Node can load Yarn's `.js` as an ECMAScript module and fail with `Dynamic require of "util" is not supported`.

For a Yarn Berry project, this setup uses a cache under `cache_dir` while installing dependencies into the workspace:

```toml
[egress]
yarn = ["registry.npmjs.org", "registry.yarnpkg.com"]

[setup]
command = ["sh", "-c", "YARN_GLOBAL_FOLDER=/var/cache/paddock/yarn/global yarn install --immutable"]
cache_key = "yarn.lock"
cache_dir = "/var/cache/paddock/yarn"
```

Select `yarn` in the setup profile's `network.setup`. `YARN_GLOBAL_FOLDER` relocates Yarn's global cache under the setup cache directory. Yarn Berry lockfiles do not contain registry URLs, so you cannot derive these hosts from the lockfile. Add any other hosts the project needs. Pick an install mode that leaves the project's run dependencies in the workspace. The global setup cache is not mounted during run.

## Avenor launch wrapper

An Avenor caller can use this command prefix, substituting its profile, spec, and workspace:

```text
/usr/bin/env paddock run --profile <p> --spec <s> --workspace <dir> --
```

The caller appends the agent command and arguments. If its current directory resolves inside the workspace, that directory is the guest working directory. Otherwise paddock uses the workspace root. This directory also decides where `setup.cache_key` resolves and which Claude transcript directory is mapped. Pass `--spec` explicitly for unattended launches.

For every key, default, and validation rule, see the [configuration reference](config.md).
