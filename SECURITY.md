# Security

## Threat model

paddock runs one command, typically a coding agent, in an Apple `container` VM with explicit mounts, a
dropped-privilege user, and an nftables egress allowlist enforced by an SNI proxy. The agent reads
untrusted text (issues, pull requests, source files, web content) and may be prompt-injected, so paddock
treats everything the agent does inside the guest as attacker-influenced. The sandbox is meant to
bound what an injected agent can read, where it can send data, and what it can change on the host.
Credentials are resolved at launch and held only by the guest process tree that needs them; they
never appear in argv, the plan, init config, or error messages.

Inside the guest, `paddock-init` drops to an unprivileged uid (capabilities cleared, `no_new_privs`),
runs the allowlist proxy as a separate uid, and installs a firewall the agent cannot flush. The checks in
`internal/guest/verify` (`paddock run --verify`) assert the properties below at run time.

## What verify covers

- Privileges are dropped, and each read-only mount refuses a write while each writable directory accepts one.
- Denied hosts, raw IPs, other ports, IPv6, and external or pinned resolvers are refused.
- Names outside the allowlist do not resolve, and `/etc/hosts`, `/etc/nsswitch.conf` and
  `/etc/resolv.conf` refuse writes from the agent.
- An allowlisted host connects through the proxy.

## Residual risks

1. **`api.anthropic.com` is a write channel.** An allowlisted host that accepts authenticated requests
   can carry data out: a request made with an attacker's API key leaves the sandbox as ordinary
   TLS to an allowed name. Closing this needs a credential-injecting, TLS-terminating proxy, which v1
   does not have. The same holds for any other allowlisted host that accepts authenticated writes,
   such as an OpenTelemetry metrics endpoint.
2. **A read-write workspace is untrusted output.** The agent can write `.git/config` (fsmonitor,
   `core.sshCommand`), git hooks, `.envrc`, package scripts and similar files that run later with the
   operator's privileges. Review the workspace before running anything from it on the host.
3. **The transcript directory is read-write on the host.** The agent can forge session files or plant
   symlinks there. Treat transcripts as untrusted input.
4. **Spec selection reads the git origin, which the agent can write.** An agent that changes the
   workspace's remote can steer which spec a later run selects. Unattended callers should pass
   `--spec` explicitly.
5. **Deny rules in managed settings are not a boundary.** The agent's own deny rules and hooks are
   advisory: `git -C repo push -f` or `git push origin +HEAD:main` can get past a pattern, and
   `python3` with `urllib` gets past a `curl` deny. The mounts, credentials, and egress allowlist
   are the boundary.
6. **Repository-trust specs are not loaded in v1.** `checkSpec` already rejects egress sets and
   managed settings (and with them allow rules and hooks) from them. Before they are loaded, it must
   also block `context.readonly` host mounts, `setup.command`, `setup.cache_dir`/`setup.cache_key`,
   and image Dockerfiles.
7. **The setup cache is shared between workspaces.** The host cache directory is keyed on the spec
   ref and a hash of the workspace's `setup.cache_key` file, and the workspace controls that file:
   copying another project's lockfile gives the same key. The setup phase mounts the cache
   read-write and runs the workspace's package scripts (risk 2), so a setup in one workspace can
   plant entries that a later setup of another workspace with the same spec and lockfile reads. A
   cache hit is only reported; the setup command still runs. Use install commands that check cached
   packages against the lockfile's checksums, do not run setup for an untrusted workspace under a
   spec that trusted workspaces use, or clear `~/.cache/paddock/<spec>`.
8. **Image pruning is not coordinated between runs.** After it builds or reuses a spec's image
   chain, paddock removes the other tags of that chain's repositories. Two concurrent runs of one
   spec whose Dockerfile or build context changed between them can remove the image the other run
   has prepared but not yet started, and that run's `container run` fails. Running it again
   recovers. A container that is already running keeps its image.
9. **The setup cache lock ends with the paddock process, not with its VM.** `paddock setup` holds
   an exclusive lock on the cache while the setup command runs. If paddock is killed, the lock is
   released at once, but the VM keeps writing the cache until the watchdog stops it, a few seconds
   later. A setup started in that window can write the same cache at the same time and leave a
   mixed package tree. The killed run never marks the cache complete, and every setup runs its
   install command again (risk 7), so an install that checks cached packages against the lockfile
   repairs the tree on the next setup. Clearing `~/.cache/paddock/<spec>` also recovers.

## Notes on the network policy

- DNS is permitted only from the proxy uid. The agent's lookups cannot reach a resolver, so a name
  cannot carry data out. `paddock-init` writes a root-owned `/etc/hosts` that maps each allowlisted host
  to a proxy address, and the proxy resolves the SNI name itself through the pinned resolver.
- The allowlist is exact host names. There are no wildcards, IP literals, or `.invalid` and
  `.localhost` names, because a wildcard would let the agent make the proxy look up names of its choosing.
- The proxy refuses a ClientHello that carries Encrypted Client Hello (ECH), because ECH hides the name
  the client is really asking for behind an outer name. Chromium sends ECH-GREASE by default, so a
  Chromium-based browser, including headless Chrome under Playwright, is refused under the allowlist
  policy. Run browsers on an open-egress profile instead.
- The proxy's denial log is diagnostic, not tamper-evident. It lives on a read-write mount that any
  guest uid can write, so the agent can alter or fill it. Do not use it as an audit record.
