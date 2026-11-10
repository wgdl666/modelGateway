# CN registered endpoint deployment

## Runtime contract

Ops owns the endpoint, health URL and physical GPU resource group of each instance.
The Go gateway selects a protocol by model key and sends requests directly to that
registered endpoint. There is no per-GPU ACK adapter, address table, or new ACK
Service port. Private IPs use the existing CIDR policy. Public HTTPS endpoints use
an approved hostname rule and allowed port; DNS answers are checked at connection
time and pinned, and TLS verifies the registered hostname.

Use `cn-native.config.json` as an overlay onto the existing CN Nacos configuration,
preserving the queue, timeouts, result bucket, and unrelated synchronous upstreams.
The example domain rule permits AutoDL's `seetacloud.com` hosts; narrow it when the
set of trusted providers is smaller. Adding a model instance within that policy
requires only Ops registration. New protocols or new trusted providers require a
configuration change, not a per-GPU adapter deployment.

`ack.yaml` references `model-gateway-native` (optional while migrating). Supply
`MODEL_GATEWAY_NATIVE_IMAGE_USERNAME`, `MODEL_GATEWAY_NATIVE_IMAGE_PASSWORD`, and
any image/video `TOKEN` needed by the models, plus `OSS_ACCESS_KEY_ID` and
`OSS_ACCESS_KEY_SECRET`, through that Secret. Never put credentials in endpoint
URLs, checked-in files, browser configuration, or command-line arguments.

An image endpoint can be `https://HOST:8443/vworld/gpu1`, with `/health` appended.
A video endpoint can be `https://HOST:8443/gpu0`, with `/healthz` appended. A dedicated
single-GPU port can instead use a root endpoint. Each video health response must
report exactly one worker and the registered physical GPU. Outfit and BodyModel
on the same card use the same resource group and total capacity 1.

## Release and acceptance

1. Record the reviewed commit and immutable image digest. Export the live Go and
   adapter Deployments, Services, Nacos config, instance registrations, resource
   groups and referenced Secret names to a restricted release directory. Keep
   old images, adapter state, object storage and all request history.
2. Run `go test -race ./... -count=1` with an isolated PostgreSQL `_test` database
   and RabbitMQ, then `go vet ./...`. Review changes since the running commit.
   The `dev` push workflow deploys both CN and AWS: use an explicit CN-only release
   for this migration and do not trigger a cross-region release inadvertently.
3. Pause the affected instances in Ops and wait for `reserved_requests == 0`.
   Verify model execution is also idle. Do not delete or force-release requests.
4. Stop all old Go gateway replicas before changing protocol selection: adapter
   endpoints and native endpoints use different protocols. Keep the adapter
   deployment for rollback. Populate the native Secret, publish the reviewed
   config, and start the reviewed Go image with the existing replica count.
5. Edit the existing registrations through Ops/API to the native endpoints and
   health URLs. Preserve IDs, physical GPU groups and capacity 1. Probe each
   instance before resuming. Background probing skips drained registrations;
   use the manual probe API during the maintenance window.
6. Resume and submit one real request to every instance, controlling availability
   when necessary to select a card. Verify the persisted request endpoint,
   instance ID, output object bytes and `slot_reserved == false` on completion.
   Test Outfit and BodyModel together on a shared group to verify serialization.
7. Only after acceptance, scale `model-gateway-worker` to zero. `ack-worker.yaml`
   defaults to zero for this reason; do not apply it before draining and switching.
   Preserve its live snapshot and state for rollback. New GPU additions do not
   change either ACK manifest. Record the final config and acceptance evidence.

## Rollback

Pause affected registrations and wait for all native executions and reservations
to finish. If execution is uncertain, preserve the reservation and inspect model
state; do not force-free the card. Restore the saved adapter Deployment/Service,
its image and state. Stop the Go gateway, restore the saved Nacos configuration
and Go Deployment/Secret references, restart, then edit registrations back to the
saved adapter endpoints using fresh `config_revision` values. Probe and resume.
Never restore the removed wgdl-dev Nginx relay or its security-group opening.

## Boundaries

This change preserves the repository's existing MQ retry behavior. An interrupted
native image submission can be retried with the same request ID, but the image
service must enforce its local GPU lock; identical IDs alone do not guarantee
idempotent inference. Gateway capacity controls dispatch, not cancellation of an
unknown running request. The CN model services' single-card admission guard must
remain enabled. The separate video Gemma handoff/memory release has its own
review and rollback; endpoint migration does not require restarting GPU models.
