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

The internal extension is enabled only when `CONTINUITY_INTERNAL_API_SECRET`
is non-empty. An invalid configured secret leaves the mounted API fail-closed;
an absent secret leaves the routes unmounted.

Account API relay binding has an independent, default-off data-plane gate. It
requires `CONTINUITY_ACCOUNT_API_ENABLED=1` (the exact string `1`) and a valid
`CONTINUITY_INTERNAL_API_SECRET`. Setting the internal secret alone mounts the
management/finality routes but does not enable external Account API relay
traffic. This separation lets operators stage policy and retain finality reads
for already-forwarded requests while the data plane is disabled.

The model adapter remains in `model/continuity_managed_groups.go` because it
must use NewAPI's cross-database transaction and cache primitives. It is the
business adapter between the extension and NewAPI's private model layer.

The data-plane binding uses the generic optional
`extension.RelayV1MiddlewareProvider` seam. The Continuity plugin contributes
middleware to `/v1` only while `CONTINUITY_ACCOUNT_API_ENABLED=1` and the
internal secret is valid; that middleware is otherwise absent. While enabled,
it remains inert unless private request-binding headers are present. This keeps
the core relay router unaware of the Continuity package.

The only active-probe relay seam is `controller.ProbeChannel`. It reuses
NewAPI's existing channel-test adaptor, model-mapping and response-validation
path for one exact group/model/channel tuple, while deliberately skipping
billing, consume-log creation, channel response-time mutation and automatic
enable/disable logic.
Keep that exported seam narrow when resolving upstream changes; the probe
scheduler and status policy belong in `extension/continuity/`.

Synchronous `/v1` relays emit one generic, privacy-minimal
`extension.RelayOutcomeEvent` after the request's final retry result is known.
The event contains only the exact routing group, original model, completion
time, latency, success flag and whether the request reached a health-relevant
routing stage; it contains no user, token, request id, prompt, response, error
text or channel credential data. Continuity keeps an immediate bounded index
and asynchronously persists per-minute aggregate counts. Local validation,
billing and policy rejections are counted separately but do not mark an
upstream pair unhealthy. Asynchronous task submission is not treated as a
completed-request success because provider acceptance does not prove the
eventual task outcome.

## Compatibility contract

`GET /internal/continuity/capabilities` is the versioned compatibility gate.
Protocol version `1` always advertises these Account API control-plane
capabilities while the internal extension is mounted:

- `account_api_requests.finality.read`
- `account_api_tokens.disable`

It advertises `account_api_requests.trusted_binding.v1` only when
`CONTINUITY_ACCOUNT_API_ENABLED=1` and the internal secret is valid. Gateway
must require that capability before creating/reusing an Account API token or
admitting a request. Turning the relay gate off removes only this capability;
`account_api_requests.finality.read` remains available so historical holds can
settle safely.

The remaining protocol-version-1 capabilities and limits are:

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

Account API inference requests carry a timestamped HMAC over the exact NewAPI
owner, token, request id, POST method and URL path. The extension accepts the
binding only for the fixed Account API token domain, removes all private
headers before relay, replaces and echoes `X-Oneapi-Request-Id`, and inserts a
durable processing record before downstream work. Each successful synchronous
error/consume log write is appended immediately to a request-local collector.
After the relay chain returns, the extension atomically freezes that collector
as `finalized`. It never treats an immediate read-after-write query as proof of
absence, because a separate log database or ClickHouse may delay visibility.
A log write/collection failure becomes `indeterminate`; it is never reported
as a finalized empty bill.

The generic token-auth guard also reserves that fixed token domain: a managed
Account API token is rejected on every token-authenticated route unless the
trusted `/v1` binding middleware has marked the request. This remains true
when the relay gate is off or the extension secret is absent, preventing a
stale unlimited hidden token from becoming directly usable during a
configuration outage. Ordinary token names never enter this guard.

The secret-authenticated finality endpoint is:

- `GET /internal/continuity/account-api/users/:userId/tokens/:tokenId/requests/:requestId/outcome`

The three path values are one exact lookup tuple. `processing` proves only that
the request was admitted, while `finalized` is the terminal evidence that can
drive settlement, including a finalized empty type-2 snapshot. Outcome storage
is migrated lazily only after a trusted request or internal outcome lookup, so
ordinary/default-off requests do not create or access this table.

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

Scheduled and manual provider probes are limited to the `standard`, `surge`,
`direct`, `wild`, `spot`, and `turbo` routing-group families. Matching is
case-insensitive against the segment before the first `-`, so deployed keys such
as `Standard-1.2` and `Turbo-2.5` remain eligible when their ratio suffix changes.
Other groups remain visible through passive user-traffic status but never send
provider probes.

Scheduled and manual checks use sanitized user traffic as the primary health
evidence. For each exact group/model pair, all relevant final relay outcomes in
the fixed preceding five-minute window form one free health sample: success-only
is green, failure-only is red, and mixed success/failure is yellow. Any of these
three observed states covers the pair for that task, so the task keeps its normal
schedule but sends a paid provider probe only when the pair has no user-traffic
evidence in the window. A successful active probe follows the same
stop-at-first-success rule. Every fallback attempt and failure confirmation
rechecks exclusions and current traffic, so evidence that arrives during a long
task can still stop further provider calls. Stream outcomes such as
`client_gone`, timeout, scanner error, panic, ping failure or a recorded soft
stream error are failures even when an adapter returned no Go error.

Recent-outcome memory is bounded to 4096 sanitized exact pairs, evicts the least
recently updated pair at capacity, and removes stale entries periodically.
Per-minute pair aggregates retain all success, relevant-failure and
ignored-local-failure counts for 48 hours without retaining user data.
Signal history pre-aggregates those rows into its existing 20-minute cells:
success-only is green, failure-only is red, and mixed traffic is yellow. A
traffic cell takes precedence over an automatic-probe point in the same cell;
the probe remains the fallback for traffic-idle cells. Scheduled task results
also persist traffic-covered evidence as a short-lived compatibility fallback
and preserve probe rotation. Check summaries expose
`traffic_covered`, `probed`, and `provider_attempts`; `provider_attempts` is the
actual number of `controller.ProbeChannel` calls and is the direct probe-cost
counter. Older task results without an evidence source continue to decode as
active-probe evidence.

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
