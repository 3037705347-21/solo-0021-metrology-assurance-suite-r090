# Metrology Assurance Suite

## Project Goal

Metrology Assurance Suite is a local HTTP service that keeps a traceable record
of laboratory measurement assurance work. It registers devices, opens a
verification case for one device, records technician measurements, requests a
repeat measurement when tolerance is exceeded, and seals the final evidence
record after review.

The service is intentionally self-contained. It uses an in-memory repository
with atomic copy-on-write mutations and does not require a database or an
external service.

## Users

- Laboratory steward: registers devices and opens verification cases.
- Measurement technician: records observed values for the queue-head open
  case.
- Quality verifier: reviews accepted measurements and seals the case.

## Core Entities

- Device: a laboratory asset with a unique asset tag, model, room, steward, and
  lifecycle state.
- VerificationCase: one bounded assurance activity for a device, with an
  optional batch, nominal value, tolerance, current state, version, assigned
  steward, promised completion time, priority with its recorded basis, and
  the immutable enqueue sequence that fixes its queue identity.
- Measurement: an observed value, computed deviation, acceptance decision,
  technician, and timestamp.
- EvidenceSeal: the immutable closing record for a reviewed case, including the
  selected accepted measurement.
- AuditEvent: an append-only trace of meaningful case and device changes.

## Workflows

### 1. Register Device

The steward sends `POST /devices` with an asset tag, model, room, and steward.
The service validates the request, normalizes the asset tag, rejects duplicate
tags, and creates an active device.

### 2. Open Verification Case

The steward sends `POST /cases` with a device ID, optional batch ID, nominal
value, tolerance, owner, priority (1–5) with the priority basis, and the
planned completion time. The service verifies that the device exists, is
active, and has no unfinished case. A valid request creates a case in
`awaiting_measurement` and allocates its immutable enqueue sequence.

### 3. Read Case And Queue

`GET /cases/{id}` returns the case with its current `queue_position` and an
`overdue` flag (true for unfinished cases past their planned completion
time). `GET /cases` returns the current measurement queue in stable
processing order, optionally narrowed by `device_id` or `batch_id`; filtered
views keep global positions so every view names the same head. The queue
orders awaiting cases by priority, then planned completion time, then
enqueue sequence.

### 4. Record Measurement

A technician sends `POST /cases/{id}/measurements`. The case must be
`awaiting_measurement` and must be the head of the queue: the gate and the
queue listing are computed from the same deterministic ordering, and the
check shares the measurement commit under the repository lock. The service
computes the signed deviation from the nominal value. An in-tolerance
measurement moves the case to `review_ready`; an out-of-tolerance
measurement moves it to `recheck_required`.

### 5. Reopen Verification Case

The steward sends `POST /cases/{id}/reopen` after an out-of-tolerance result.
Only `recheck_required` cases may move back to `awaiting_measurement`. The
case keeps its original enqueue sequence, so rework re-enters the queue
without rewriting its identity or its measurement history. The service
records the actor and reason in the audit trail.

### 6. Adjust Case Priority

The steward sends `POST /cases/{id}/priority` with a new priority, the
basis for the change, and the acting user. Unfinished cases move in the
queue immediately; the enqueue sequence, planned completion time,
measurements, and seals are untouched. Sealed cases are history and reject
the change. Every adjustment appends a `priority_adjusted` audit event.

### 7. Seal Verification

The verifier sends `POST /cases/{id}/seal` after an accepted measurement. Only
`review_ready` cases may be sealed. The service selects the accepted
measurement with the smallest absolute deviation, creates exactly one evidence
seal, moves the case to `sealed`, and rejects later mutations.

## State And Rules

Device state:

- `active -> retired`
- `retired` is terminal.

VerificationCase state:

- `awaiting_measurement -> review_ready`
- `awaiting_measurement -> recheck_required`
- `recheck_required -> awaiting_measurement`
- `review_ready -> sealed`
- `sealed` is terminal.

Required invariants:

- Asset tags are unique after case-insensitive normalization.
- A device has at most one unfinished verification case.
- Measurements are accepted only while a case is awaiting measurement.
- A tolerance must be positive.
- A case can have at most one evidence seal.
- A sealed case cannot accept measurements, reopen, seal again, or change
  priority.
- Opening a case requires a priority (1–5) with a non-empty basis and a
  planned completion time.
- Enqueue sequences are allocated once at open time and never reused or
  rewritten; reopening and priority changes keep the original sequence.
- The measurement queue contains only `awaiting_measurement` cases, ordered
  by priority, planned completion time, then enqueue sequence.
- Only the first case of that ordering may record a measurement, checked in
  the same atomic commit that stores the measurement.
- New cases, completed cases, rework, and priority changes append to the
  audit trail and never modify earlier measurements, seals, or events.
- Every mutation that changes a case also appends an audit event in the same
  repository commit.

## Modules

- `cmd/metrology-service`: process entrypoint and graceful HTTP shutdown.
- `cmd/workflowcheck`: bounded smoke checks that call the public HTTP API.
- `internal/httpapi`: routing, request decoding, and stable error mapping.
- `internal/assurance`: workflow orchestration and atomic decisions.
- `internal/domain`: entities, validation, state transitions, and errors.
- `internal/repository`: mutex-protected in-memory persistence with copy-on-write
  commits.
- `internal/contracts`: request and response contracts shared by the HTTP layer.

## Public Interfaces

- `GET /healthz`
- `POST /devices`
- `GET /devices/{id}`
- `POST /cases`
- `GET /cases`
- `GET /cases/{id}`
- `POST /cases/{id}/measurements`
- `POST /cases/{id}/reopen`
- `POST /cases/{id}/priority`
- `POST /cases/{id}/seal`
- `GET /cases/{id}/events`

## Validation Plan

The baseline build command is `go build ./...`. Each declared workflow has a
bounded production smoke command under `cmd/workflowcheck`. The command starts
an HTTP test server in the same process, calls the public routes, and verifies
both success and failure behavior. The seal check also races two concurrent
seal requests to prove that the repository mutation lock permits exactly one
seal. The queue check proves the listing head is the only case that passes the
measurement gate, that overdue status is reported without reordering work, and
that a reworked case keeps its enqueue sequence. The priority check proves a
promotion reorders the queue and appends an event while a sealed case rejects
reprioritization and keeps its history.

Tests are intentionally deferred for this initialization baseline. The later
engineering-task stage will add unit and integration tests, including red/green
verification for the workflows and their failure paths.
