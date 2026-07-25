# Continuity extension maintenance

The Continuity integration is a source-level extension carried by the
`Faye-labs/new-api` fork. It is intentionally not a Go `.so` plugin: Go's
dynamic plugin ABI is tied to the exact toolchain and dependency graph, which
makes it less reliable across frequent upstream NewAPI updates.

## Extension boundary

The fork has one generic host mount in `router/main.go`:

```go
extension.MountAll(router)
```

Fork-specific imports live in separate files such as
`router/continuity_extension.go`. The Continuity transport, authentication,
validation, protocol and routing-group catalog live under
`extension/continuity/`. Removing that import and package removes the
extension without editing NewAPI's API router.

The extension is enabled only when `CONTINUITY_INTERNAL_API_SECRET` is
non-empty. An invalid configured secret leaves the mounted API fail-closed;
an absent secret leaves the routes unmounted.

The model adapter remains in `model/continuity_managed_groups.go` because it
must use NewAPI's cross-database transaction and cache primitives. It is the
business adapter between the extension and NewAPI's private model layer.

The only relay seam is `controller.ProbeChannel`. It reuses NewAPI's existing
channel-test adaptor, model-mapping and response-validation path for one exact
group/model/channel tuple, while deliberately skipping billing, consume-log
creation, channel response-time mutation and automatic enable/disable logic.
Keep that exported seam narrow when resolving upstream changes; the probe
scheduler and status policy belong in `extension/continuity/`.

## Compatibility contract

`GET /internal/continuity/capabilities` is the versioned compatibility gate.
Protocol version `1` advertises:

- `group_model_status.read`
- `group_model_status.checks.read`
- `group_model_status.checks.write`
- `group_model_status.exclusions.read`
- `group_model_status.exclusions.write`
- `routing_groups.read`
- `token_groups.batch_write`
- `user_group.write`
- group keys limited to 64 UTF-8 bytes
- token mutation batches limited to 100 items
- `single_process_population_fence_v1` cache coherency

The `@continuity/api` client verifies this response before its first operation
and fails closed on a missing or incompatible extension.

The status integration uses these secret-authenticated endpoints:

- `GET /internal/continuity/group-model-status` returns the current exact
  group/model matrix with passive and recent active evidence.
- `GET /internal/continuity/group-model-status/checks` returns a sanitized view
  of the active or latest check task.
- `POST /internal/continuity/group-model-status/checks` enqueues a manual check
  or returns the existing single-flight task.
- `GET /internal/continuity/group-model-status/exclusions` returns the persisted
  exact group/model pairs omitted from active checks and status snapshots,
  together with an `initialized` readiness flag.
- `PUT /internal/continuity/group-model-status/exclusions` atomically replaces
  that list. The request must contain a non-null `pairs` array; use an explicit
  empty array to initialize checks with no exclusions.

Each exclusion is scoped to one exact group/model pair. An identical model ID
in another group remains visible and probeable. Stored exclusions remain
readable after a routing group is removed so routine group lifecycle changes
cannot break status or scheduled checks. A replacement may preserve such an
exact stale pair while changing other groups, but cannot introduce a new pair
for an unknown group. A running check re-reads exclusions before every channel
attempt, so a newly excluded pair is not tried on a later fallback channel.
Until the option has been explicitly initialized by a PUT, scheduled and manual
checks complete with zero pairs and make no provider calls. The status snapshot
remains available for passive traffic projection during that readiness state.

Active checks are scheduled every 20 minutes by default. Override the cadence
with `CONTINUITY_GROUP_MODEL_PROBE_INTERVAL_MINUTES`, or disable scheduled
checks with `CONTINUITY_GROUP_MODEL_PROBE_ENABLED=0`; manual checks remain
available while the extension itself is enabled.

`single_process_population_fence_v1` means the fork prevents a DB read started
before a managed mutation from repopulating stale data in the same NewAPI
process. Do not scale multiple NewAPI processes against one Redis instance
until the contract is upgraded to a Redis generation/CAS guard and the client
accepts that new contract.

## Upstream update workflow

Keep upstream history and fork changes in separate commits:

1. Sync the unmodified `QuantumNous/new-api` history.
2. Apply the generic cache-coherency commit.
3. Apply the generic extension registry and Continuity host adapter.
4. Apply the Continuity extension package.

The recommended remote layout is:

```text
upstream  https://github.com/QuantumNous/new-api.git
origin    https://github.com/Faye-labs/new-api.git
```

After merging or rebasing a new upstream release, run:

```bash
go test ./extension/... ./controller ./model ./router
```

Then build the normal NewAPI artifact and probe the deployed capability
endpoint before enabling Gateway model-brand routing. A compile failure in
the model adapter or a capability mismatch is an explicit upgrade stop; do
not bypass it with direct database writes.
