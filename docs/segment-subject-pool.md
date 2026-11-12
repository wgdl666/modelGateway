# Optional SegmentSubject logical pool

The default configuration and existing loopback dispatch remain unchanged. An
opt-in pool is one aggregate ledger instance, with one endpoint budget and one
physical GPU resource group. It does not identify which replica an ALB selected.

```json
{
  "subject_pools": {
    "Vmind_segment_subject_bria_v1.0": {
      "protocol": "segment_subject",
      "endpoint": "http://private-kong.example.internal/internal/segment-subject-pool",
      "health_url": "http://private-kong.example.internal/internal/segment-subject-pool/ready"
    }
  }
}
```

Only this exact model and protocol are supported. Registration must match both
configured URLs exactly. Add the Kong port to `backend_ports` and its private IP
ranges to `backend_cidrs`. DNS answers are checked at connection time, every
answer must be private and inside those CIDRs, and the transport dials the checked
numeric addresses. Public-host exceptions do not override this pool policy.

Inject `MODEL_GATEWAY_SUBJECT_POOL_API_KEY` from the deployment secret store. It
is required with a configured pool, is not accepted from JSON or serialized in
configuration, and is sent as `X-Segment-Pool-Key` only on matched pool execution
and readiness probes. Configure Kong key authentication on that header, with no
inference retries. Requests go to `<endpoint>/api/segment_subject` and readiness
to `<endpoint>/ready`; Kong must strip the prefix. Legacy requests continue to
use their original `/_dispatch/<port>` targets without this key.

Pool maps and credentials cannot be changed by runtime reload. Drain, preserve
uncertain reservations, and restart when changing these bindings. Keep a target
configured while reservations still reference it. A pool readiness 200 indicates
available ingress, not proof that all ECS targets are healthy; monitor target
count externally.

Register one new immutable deployment ID, with the exact image artifact digest,
real machine ID, GPU device, and shared physical resource group. For two admitted
requests use instance capacity 2, group capacity 2, and model units 1 only after
concurrent GPU validation. Do not register separate fictional replica rows at
the same load-balanced endpoint.

The optional registration field `initially_draining: true` atomically creates a
staged row using the existing draining column. Omitted defaults to false. Exact
registration retries retain the row's current drain state, even after an operator
resumes it. Use the normal drain API to activate; registration retries cannot
resume or re-drain existing deployments. Existing immutable identity checks and
same-endpoint reservation protections remain in force.

A complete EOF with `X-Model-Outcome: unknown`, or a lost response after request
headers were written, retains ledger reservations for explicit reconciliation.
The pool runtime and Kong must preserve that header on an active-work timeout.

## AWS release persistence

The exhibition release script updates `ImageDigest` with CloudFormation
`--use-previous-template`, preserving every other stack parameter with
`UsePreviousValue`. Registering an ECS task definition by hand does not update
that canonical stack template; a later CI release will recreate the stack's
original secret list.

Before enabling the pool in AWS, update the `tomirro-model-gateway` stack template
with optional string parameter `SubjectPoolSecretArn` (default empty) and a
condition testing that it is nonempty. Under that condition only:

- Append container secret `MODEL_GATEWAY_SUBJECT_POOL_API_KEY` with `ValueFrom`
  referencing `SubjectPoolSecretArn` to the `model-gateway` container in `Task`.
- Grant `secretsmanager:GetSecretValue` for exactly `!Ref SubjectPoolSecretArn`
  in the `ExecutionRole` inline policy. Do not grant this to the CI release role
  or the application task role. Do not use a wildcard secret resource.

Use `AWS::NoValue` for both disabled branches. Supply the standalone secret ARN
as a stack parameter; keep its value out of templates and configuration files.
This one-time stack preparation includes an IAM policy change and must be
reviewed separately from the ordinary image-only CI change set. Once applied,
the existing release script preserves the template and parameter without
additional CI secret access or changes to its two-resource image-update guard.
The full exhibition ECS stack template lives outside this repository; the
release-role template in `deploy/` is not the application stack template.
