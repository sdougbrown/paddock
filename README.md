# paddock

Run one command in a sandbox with explicit mounts, limits, and an egress allowlist.

```sh
make build
PADDOCK_CONFIG_DIR=./internal/resolve/testdata/root bin/paddock plan --profile fixer --workspace <dir> -- claude
```

`paddock plan` prints the resolved plan as JSON. `--workspace` defaults to the current directory;
the fixer profile mounts `repo` from the workspace read-only, so the directory must contain a `repo/`. Config lives in `~/.config/paddock`
(`profiles/<role>.toml`, `projects/<owner>[/<repo>].toml`). See `AGENTS.md` for the trust rules and `SECURITY.md` for the threat model and residual risks.

## Install

Download the latest release archive and extract it into a directory on `PATH`:

```sh
gh release download --repo sdougbrown/paddock --pattern paddock_darwin_arm64.tar.gz
mkdir -p ~/.local/bin
tar -xzf paddock_darwin_arm64.tar.gz -C ~/.local/bin
```

The binaries are not notarized. `gh` and `curl` downloads carry no quarantine attribute; for an archive
downloaded in a browser, run `xattr -d com.apple.quarantine ~/.local/bin/paddock` before the first run.

The archive holds `paddock` and `paddock-init` (the linux/arm64 guest entrypoint). paddock looks for
`paddock-init` beside its own executable; set `$PADDOCK_INIT` to use one elsewhere.

## Image layers

A spec may ship `images/<spec>/Dockerfile`. paddock builds the base image `paddock/base`, then each spec layer in
chain order, passing the previous image as the build argument `PADDOCK_PARENT`. A layer's
Dockerfile must start with:

```dockerfile
ARG PADDOCK_PARENT
FROM ${PADDOCK_PARENT}
```

Layer tags are `paddock/<lowercased spec, "/" as "-">-<ref8>:<hash>`, where `ref8` is the first 8 hex
digits of the sha256 of the spec ref, so every spec has its own repository. The hash covers the Dockerfile,
the build context (symlinks in the context path are resolved first), and the parent tag. Layers
whose tag already exists are not rebuilt. After the chain is built or reused, paddock removes other
tags of `paddock/base` and the chain's layer repositories; images in use, and all other images, are left alone.

## Setup cache

`paddock setup` mounts a host cache directory at the spec's `setup.cache_dir`. The directory is
`~/.cache/paddock/<lowercased spec ref>/<hash of the setup.cache_key file>`, so the cache is shared
per spec and lockfile across workspaces; Yarn verifies package checksums against the lockfile. The
completion marker is `<cache dir>.complete`, beside the mounted directory, and is cleared before each
setup run. `setup.cache_dir` must be a clean absolute guest path strictly under
`/var/cache/paddock/` and may not equal, contain, or lie under another plan mount. Setups for the
same cache serialize on an exclusive flock of `<cache dir>.lock`, held until the setup run ends.
The setup phase uses the profile's `workspace.mode`; a profile whose workspace is `ro` cannot run
setup.

## Claude Code inside paddock

Turn Claude Code's own sandbox off for a command that runs in a paddock: settings written for it set
`sandbox.enabled: false`. paddock's mounts, egress allowlist, and dropped privileges, together with the
caller's permission hook, are the boundary. Claude Code's Linux sandbox needs bubblewrap and socat,
which paddock images do not ship, and it cannot mount a fresh /proc inside the guest.
