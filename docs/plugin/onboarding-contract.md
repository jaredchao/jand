# Relay token onboarding — local tools, no production activation

The existing Relay accepts SHA256 hashes in `access.tokens_sha256` for creation
requests. Empty means **public access**, not deny-all. There is no registration,
application, quota or token-management HTTP API. `/healthz` GET exposes public
`access: true/false`; it does not validate a token. Config is read at startup,
not dynamically reloaded. Restart loses all in-memory invitations/chats; plan a
window or drain sessions before a future authorized deployment.

## Plugin first-use flow

1. Select a supported runtime from the bundled pinned runtime manifest and verify
   both archive and binary hashes. Published v0.4.6 supports the five listed
   platforms; Windows arm64 is unsupported. Do not invoke inspect against 0.4.6.
2. Use the selected Relay's `/healthz` for availability/access requirements. When
   creating a handoff/chat on access=true, ask the user for their personal token
   from that Relay's administrator. Never ship a shared token or use the existing
   private integration token for general users. Receiving an invitation and
   joining by its capability do not require issuing a separate access token.
3. Store the user's token in a private token file (0600 on Unix, user-only ACL on
   Windows), configure `access_token_file` for the plugin's dedicated JAND_HOME,
   or pass it only through child environment `JAND_ACCESS_TOKEN`. Do not put raw
   tokens in command arguments, logs, settings pages, telemetry, snapshots,
   manifests or source. Environment overrides token_file, so sanitize inherited
   environment when selecting a file-based configuration.
4. Existing `jand config --json` exposes token source, never token value. Missing,
   unreadable or empty configured token files are errors. A configured source
   does not prove a valid/enabled token. Creation HTTP 401 means missing/unknown
   token; ask for a corrected token/operator action. Do not silently retry using
   a shared credential. Network failure is different from missing/invalid token.

The existing setup command can configure a token from a file, but setup also
writes agent integration and normally performs a send self-test. The plugin
should manage its dedicated config explicitly or request that broader setup
intentionally; no UI, global install or production change is implied here.

## Operator workflow (offline)

Tool: `python3 scripts/relay_tokens.py --registry PATH <action>`. Registry path
must have an existing private parent directory. Registry is schema 1, hash-only,
with stable non-secret subject IDs and issued/revoked timestamps. It preserves
revoked history; token plaintext is written only to a newly created private
file. One active token per subject; no identity verification is claimed.
Do not confuse the coordination directory with a place to store issued tokens.

Example, in a NEW private operator scratch directory (not the live config):

```sh
python3 scripts/relay_tokens.py --registry /PRIVATE/registry.json issue \
  --subject person-001 --token-out /PRIVATE/person-001.token
python3 scripts/relay_tokens.py --registry /PRIVATE/registry.json list
python3 scripts/relay_tokens.py --registry /PRIVATE/registry.json export \
  --base /PRIVATE/relay-base.json --out /PRIVATE/relay-candidate.json
bin/jand relay --config /PRIVATE/relay-candidate.json --print-config
```

Deliver each token to its intended person through an approved private channel.
No token is printed to stdout. Do not forward it to the other development chat.
Candidate generation preserves every unmanaged base allowlist hash and other
configuration fields. Validate the candidate using the actual target Relay
binary (`--print-config`, local only) before a separately authorized deployment.
Never use `--config candidate` without `--print-config` during review unless
explicitly starting a temporary local Relay.

Revoke a managed subject, then export against the latest deployed/base config:

```sh
python3 scripts/relay_tokens.py --registry /PRIVATE/registry.json revoke --subject person-001
python3 scripts/relay_tokens.py --registry /PRIVATE/registry.json export \
  --base /PRIVATE/relay-base.json --out /PRIVATE/relay-revoked-candidate.json
```

This removes that managed hash even if it appears in the base. It preserves
unmanaged hashes, including current test/operator tokens. Revocation is local
until the reviewed candidate is deployed and Relay restarted. A second active
token cannot be issued for the same subject before explicit revocation. A
revoked subject can receive a replacement into a new token file; existing token
files/config outputs are never overwritten. If export would result in an empty
allowlist, it fails (to avoid accidentally making Relay public). Stop Relay or
retain a separate operator credential rather than exporting an empty allowlist.

Mutating operations serialize on `<registry>.lock` directory and replace the
registry atomically with mode 0600. A stale lock fails closed; verify no operation
is running before manually removing it. Keep a secure operator backup of the
hash-only registry and base configuration. Protect locally issued token files;
plaintext is not recoverable from the registry. The local prototype does not
provide server-side per-subject identity, quota, expiry, approval or admin auth.
Any future self-service system needs a separately reviewed persistence and
permission design before enabling a registration endpoint.

Validation: Python tests cover one active token/person, existing-file refusal,
missing/damaged registry, hash-only storage, unmanaged-token preservation and
empty-allowlist refusal. `scripts/plugin_smoke.py` issues a temporary token,
validates candidate via the actual Relay loader, proves authorized creation and
invite receipt/join without token, restarts the temporary Relay on a revoked
candidate, proves revocation and retained unmanaged access. Existing production
and existing private test token are untouched.
