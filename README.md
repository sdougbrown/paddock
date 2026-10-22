# paddock

Run one command in a sandbox with explicit mounts, limits, and an egress allowlist.

```sh
make build
PADDOCK_CONFIG_DIR=./internal/resolve/testdata/root bin/paddock plan --profile fixer -- claude
```

`paddock plan` prints the resolved plan as JSON. Config lives in `~/.config/paddock`
(`profiles/<role>.toml`, `projects/<owner>[/<repo>].toml`). See `AGENTS.md` for the trust rules.

## Image layers

A spec may ship `images/<spec>/Dockerfile`. paddock builds `paddock-base`, then each spec layer in
chain order, passing the previous image as the build argument `PADDOCK_PARENT`. A layer's
Dockerfile must start with:

```dockerfile
ARG PADDOCK_PARENT
FROM ${PADDOCK_PARENT}
```

Layer tags are `paddock/<lowercased spec, "/" as "-">:<hash>`, where the hash covers the Dockerfile,
the build context (symlinks in the context path are resolved first), and the parent tag. Layers
whose tag already exists are not rebuilt.

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
