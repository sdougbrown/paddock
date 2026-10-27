# Configuration reference

A profile controls what a command can access. A project spec supplies the image layers, environment, and named egress sets that the profile can select. Use [the walkthrough](profiles.md) to build a profile and spec before using this page as a reference.

## Config root and spec selection

paddock chooses the first nonempty location:

1. `PADDOCK_CONFIG_DIR`
2. `$XDG_CONFIG_HOME/paddock`
3. `~/.config/paddock`

Paths in `PADDOCK_CONFIG_DIR` can be relative to the caller's current directory. The root is operator-owned, including any files it references.

```text
<root>/
  profiles/<role>.toml
  projects/<owner>.toml
  projects/<owner>/<repo>.toml
  images/...                  # optional Dockerfiles and build contexts
  claude/...                  # optional managed-settings JSON
```

Profile names and spec reference components start with an American Standard Code for Information Interchange (ASCII) letter or digit. Letters, digits, `.`, `_`, or `-` follow; `..` is forbidden.

A spec reference has one or two components. Unknown TOML keys are errors. Host hooks are not supported in profiles or specs.

Selection order:

1. Explicit `--spec`.
2. The workspace's Git `origin`, if its URL yields `owner/repo` and that spec exists.
3. The profile's `default_spec`.

A matching spec that exists but is invalid is an error, not a fallback. If none is selected, resolution fails. A writable workspace can change the origin, so pass `--spec` explicitly from unattended callers.

Repository-committed specs are not loaded in v1. The resolver rejects egress sets and managed settings in a repository-trust spec and requires it to extend an operator-trust spec. It does not restrict image layers, setup fields, or `context.readonly`; see [Security](../SECURITY.md).

## Profile keys

All defaults below apply when the key is omitted. `engine` is required.

| Key | Type and default | Validation and effect |
| --- | --- | --- |
| `engine` | string; no default | Must be `"apple-container"`. Selects the Apple `container` engine. |
| `default_spec` | string; empty | Valid spec reference. Used only after origin selection fails. |
| `workspace.mode` | string; `"ro"` | `"ro"` or `"rw"`. Mounts the workspace at the same path in the guest, read-only or read-write in both phases. Setup requires `"rw"`. |
| `workspace.readonly` | array of strings; `[]` | Relative paths inside the workspace. Each must resolve to an existing directory inside it, including after symlink resolution. Mounts each at its host path read-only, even when the workspace is writable. |
| `mounts.readonly` | array of strings; `[]` | Absolute host paths or `~/` paths. Mounts each read-only at the same guest path. A path cannot equal or contain the workspace. |
| `network.policy` | string; `"none"` | `"allowlist"`, `"none"`, or `"host"`; see below. |
| `network.run` | array of strings; `[]` | Names of spec egress sets for the run phase. With `allowlist`, at least one set and at least one resulting host are required. |
| `network.setup` | array of strings; `[]` | Names of spec egress sets for setup, subject to the same allowlist requirements. |
| `env.<NAME>` | string; absent | Literal, `file:<absolute-or-~/path>`, or `env:<host-variable>`. See [Environment](#environment). |
| `limits.cpus` | integer; `0` | Nonnegative. A positive value is passed to `container run`; zero leaves the engine default. |
| `limits.memory` | string; empty | Digits followed by `K`, `M`, or `G`, case-insensitive, such as `"6G"`. Passed to `container run`; empty leaves the engine default. |
| `claude.seed_files` | array of strings; `[]` | Absolute or `~/` host **file paths**, copied into guest `~/.claude/` by basename at launch. Basenames must be unique. Run phase only. At launch, a seed named `settings.json` is rejected when the spec chain has managed settings. |
| `claude.skills` | string; empty | Absolute or `~/` host **directory path**, inside a read-only mount: `mounts.readonly`, `context.readonly`, `workspace.readonly`, or a workspace with mode `"ro"`. Guest `~/.claude/skills` is a symlink to that same path. No symlink can intervene below the covering mount. Run phase only. |
| `claude.agents` | string; empty | Same rules as `claude.skills`; links guest `~/.claude/agents`. |

Mount paths cannot contain `:` or target paddock-managed guest paths under `/opt/paddock`, `/run/paddock`, `/run/paddock-secrets`, or `/run/paddock-log`. The engine also reserves its transcript and managed user-settings mount paths. Do not assume a read-only mount makes other mounts read-only.

## Network policies

* `allowlist` redirects guest TCP port 443 to a separate Server Name Indication (SNI) proxy. The proxy checks the exact TLS server name and resolves it through the pinned resolver. Only the proxy uid can use DNS; paddock writes allowlisted names to the guest's `/etc/hosts`. Direct DNS tools that bypass hosts, such as `dig`, do not get answers. Other ports, direct IP traffic and IPv6 are blocked. Encrypted Client Hello (ECH) is refused because it hides the server name.
* `none` allows loopback traffic only.
* `host` does not install guest network restrictions. It does not mean the workspace is mounted read-write, nor does it turn the VM into the host.

`network.policy` applies to both setup and run. With `allowlist`, the selected phase needs at least one egress set: `network.setup` for setup, or `network.run` for run. An empty list produces `allowlist policy needs at least one egress set`. For networked setup followed by an offline run, use an `allowlist` profile for `paddock setup` and a separate `none` profile for `paddock run`. See [Offline tests for a Go module](profiles.md#offline-tests-for-a-go-module).

`network.run` and `network.setup` are ignored for policies other than `allowlist`. Set names start with an ASCII letter or digit, followed by letters, digits, `.`, `_`, or `-`. A set must exist in the selected spec chain. Hosts are unioned and deduplicated. An allowed service can still receive data from the guest; see [Security](../SECURITY.md).

## Environment

Environment names follow `[A-Za-z_][A-Za-z0-9_]*`. A profile and its spec chain cannot define the same name. Spec values must be literals; neither `file:` nor `env:` is allowed in a spec. Prefixes are recognized at the start of a value, so a value beginning with either prefix is not a literal.

Literal values in profile and spec `[env]` apply to both phases. For a variable needed only during setup, wrap the setup command in a shell instead of setting it in `[env]`:

```toml
[setup]
command = ["sh", "-c", "GOMODCACHE=/var/cache/paddock/gomod go mod vendor"]
```

A profile's `file:` source reads a host file at launch and trims surrounding whitespace. It rejects an empty value, interior whitespace, and control characters. `env:` reads a named host environment variable at launch; it must be nonempty and contain no line breaks.

The plan includes source references, not secret values. paddock handles resolved secrets in four steps:

1. paddock writes them to a temporary per-run file.
2. paddock mounts the file into the guest.
3. Guest init reads and unlinks the file.
4. Guest init passes the values in the command's environment.

Secrets are not passed in command arguments or init config. Setup receives literal values only, not `file:` or `env:` credentials. The engine also passes through nonempty `TERM`, `COLORTERM`, and `LANG` unless the plan sets them.

## Spec keys

Spec files are operator-owned. Paths for image and managed-settings files are relative to the config root, not the spec file. Absolute paths and paths that escape the root are rejected.

| Key | Type and default | Validation and effect |
| --- | --- | --- |
| `extends` | string; empty | A valid parent spec reference. Parent files must exist; cycles are errors. Layers merge parent first. |
| `image.dockerfile` | string; empty | Config-root-relative Dockerfile path. Adds an image layer over the preceding layer. Declare `ARG PADDOCK_PARENT` before the first `FROM` and use `FROM ${PADDOCK_PARENT}` for the final stage. |
| `image.context` | string; empty | Config-root-relative build context; defaults to the Dockerfile's directory. Cannot be set without `image.dockerfile`. |
| `egress.<set>` | array of strings; absent | Exact DNS hostnames. No wildcards, IP literals, numeric top-level domains, `.invalid`, or `.localhost`. Names are compared case-insensitively without a trailing dot. Labels are 1–63 bytes; the full normalized name is at most 253 bytes. |
| `setup.command` | array of strings; `[]` | Command and arguments, executed directly in the guest during setup. Required for `paddock setup`. A spec file that sets `cache_key` or `cache_dir` must set `command` in the same file. No shell parsing occurs unless you explicitly invoke a shell. |
| `setup.cache_key` | string; empty | Relative path to an existing regular file of at most 64 MiB, resolved against the working directory (see [Setup cache](#setup-cache)). Its resolved symlinks must remain inside the working directory. Set together with `cache_dir` for setup. |
| `setup.cache_dir` | string; empty | Clean absolute guest path strictly under `/var/cache/paddock/`, not overlapping any other mount. Set together with `cache_key` for setup. Only setup mounts this cache read-write. |
| `context.readonly` | array of strings; `[]` | Absolute or `~/` host paths, mounted read-only at the same guest paths, with the same restrictions as profile `mounts.readonly`. This is not the image build context. |
| `claude.managed_settings` | string; empty | Config-root-relative path to a JSON object. Merged across the chain and installed for Claude Code as managed settings and read-only user settings. Run phase only. |
| `env.<NAME>` | string; absent | Literal environment value for both phases. Name rules match profile environment names. |

A child adds to its parent: it cannot redefine an egress set, environment name, or setup field. Managed-settings objects merge recursively; arrays union with parent entries first, and conflicting scalar values or types fail. Multiple image layers build in chain order. Specs cannot add arbitrary writable mounts, credentials, or host hooks.

## Image layers

The base image uses `node:22.15-bookworm-slim`. It installs Git, Python 3, nftables, iproute2, certificate authority (CA) certificates and procps, enables Corepack, and installs Claude Code 2.1.303 globally. A spec layer can install more packages with `apt-get install` at build time. Image builds run on the host, outside the guest egress policy.

Multi-stage Dockerfiles can copy tools from another image, provided the final stage uses the paddock parent. For example:

```dockerfile
ARG PADDOCK_PARENT
FROM golang:1.26 AS gotool
FROM ${PADDOCK_PARENT}
COPY --from=gotool /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"
```

paddock builds the base image `paddock/base:<hash12>`, then each spec layer in chain order. It passes the previous image tag as the build argument `PADDOCK_PARENT`. A layer's tag is `paddock/<lowercased spec, "/" as "-">-<ref8>:<hash12>`:

- `ref8` is the first 8 hex digits of the sha256 of the spec reference as written. Specs whose names differ only by case or by `/` versus `-` therefore keep separate repositories.
- `hash12` is the first 12 hex digits of a sha256 over the Dockerfile bytes, the build context tree, and the parent tag.

The tree hash covers relative paths, modes, and file contents. A symlink inside the context is hashed by its target path, not by the content it points at. A symlinked context directory is resolved first. The base hash covers only the base Dockerfile.

A layer whose tag exists is not rebuilt. After the chain is built or reused, paddock removes other tags of `paddock/base` and of the chain's layer repositories. Other images are left alone, and an image in use by a running container stays.

## Setup cache

`paddock setup` mounts a host cache directory read-write at `setup.cache_dir`:

```text
~/.cache/paddock/<spec ref>/<hash16>
```

- `<spec ref>` is the selected spec reference as written, with `/` as a directory separator.
- `<hash16>` is the first 16 hex digits of the sha256 of the `setup.cache_key` file.

The key file resolves against the guest working directory. That directory is the caller's current directory when it is inside the workspace, and the workspace root otherwise. From a subdirectory, the key is that subdirectory's file.

The cache is shared per spec and key file across workspaces. A cache hit does not skip the setup command. Run-phase commands do not mount the cache.

Dependencies stored only under `cache_dir` are invisible during run. Setup must leave what the run needs in the workspace. Examples are `node_modules` from `npm ci`, `vendor/` from `go mod vendor` with `GOFLAGS=-mod=vendor` during run, and a project-local virtualenv. See the [Go walkthrough](profiles.md#offline-tests-for-a-go-module).

To clear all caches for a spec, remove `~/.cache/paddock/<spec ref>`.

The cache is shared across workspaces, and a killed setup can leave a mixed cache while its VM stops. Use install commands that check cached packages against lockfile checksums, and do not share a spec's cache between trusted and untrusted workspaces. See [the cache risks](../SECURITY.md) before using it with untrusted workspaces.

- `<cache dir>.complete` marks a finished setup. paddock removes it before each setup run and creates it only when the command exits 0. A hit means the marker existed before this run.
- Setups for the same cache serialize on an exclusive flock of `<cache dir>.lock`, held until the setup run ends.
- `setup.cache_dir` is a clean absolute guest path strictly under `/var/cache/paddock/`. It cannot equal, contain, or lie under another plan mount.
- Setup uses the profile's `workspace.mode`. A profile whose workspace is `"ro"` cannot run setup.

## Plan output

`paddock plan` prints the resolved plan as JSON without starting a VM. Optional fields can be absent.

| JSON field | What to check |
| --- | --- |
| `profile`, `spec`, `spec_chain` | Selected profile and spec, and the spec chain. |
| `engine`, `phase` | Engine and selected `run` or `setup` phase. |
| `workspace`, `workdir` | Workspace and working directory paths. |
| `image.layers` | Spec layers in order; each has `spec`, `dockerfile`, and `context`. The base image is implicit. |
| `mounts` | Each mount's `host`, `guest`, and `read_only` fields. |
| `env` | Each variable's `name` and `source`; literals have `value` (omitted when empty), while file and environment sources have `ref`, not secret values. |
| `network` | `policy`, selected `sets`, and resolved `hosts` for the phase. |
| `setup` | When present, `command` and optional `cache_key_file`, `cache_dir`, and `cache_host`. `cache_host` is set only for setup. |
| `limits` | `cpus` and `memory`. |
| `claude` | `seed_files` and optional `managed_settings`, `skills`, and `agents`. |
| `argv` | Command and arguments. |

## Verify checks

`paddock run --verify` checks dropped privileges and the read-only and writable mounts under every policy. Its network checks depend on the policy:

| Policy | Network checks |
| --- | --- |
| `allowlist` | Firewall flush and proxy signal are refused. Protected name-resolution files refuse writes. Pinned and external DNS are refused. Unlisted names do not resolve. Denied hosts, raw IPs, other ports and IPv6 are refused. An allowed host connects through the proxy. |
| `none` | Firewall flush, IPv4 egress, IPv6 and external DNS are refused. Protected name-resolution files refuse writes. Pinned DNS is refused and unlisted names do not resolve. |
| `host` | No network probes apply. |

These checks are not a guarantee against every escape or data leak. See [Security](../SECURITY.md).

## Claude Code state

For each run, paddock prepares a guest home at the host user's home path:

- It seeds `~/.claude.json` with onboarding completion and trust for the selected working directory.
- It maps the Claude transcript directory for that working directory read-write to the corresponding host `~/.claude/projects/` directory. Treat transcripts as untrusted input.
- Seed files are copied, not mounted; skills and agents are symlinks into read-only mounts.

These conveniences do not provide an agent permission boundary; mounts, credentials, and network policy do.

Turn Claude Code's own sandbox off for commands that run in paddock: set `sandbox.enabled` to `false` in the spec's managed settings. paddock's mounts, egress policy, dropped privileges, and the caller's permission hook are the boundary. Claude Code's Linux sandbox needs bubblewrap and socat, which paddock images do not ship. It also cannot mount a fresh `/proc` in the guest.
