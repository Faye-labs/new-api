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
only business adapter between the extension and NewAPI's private model layer.

## Compatibility contract

`GET /internal/continuity/capabilities` is the versioned compatibility gate.
Protocol version `1` advertises:

- `routing_groups.read`
- `token_groups.batch_write`
- `user_group.write`
- group keys limited to 64 UTF-8 bytes
- token mutation batches limited to 100 items
- `single_process_population_fence_v1` cache coherency

The `@continuity/api` client verifies this response before its first operation
and fails closed on a missing or incompatible extension.

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
go test ./extension/... ./model ./router
```

Then build the normal NewAPI artifact and probe the deployed capability
endpoint before enabling Gateway model-brand routing. A compile failure in
the model adapter or a capability mismatch is an explicit upgrade stop; do
not bypass it with direct database writes.
