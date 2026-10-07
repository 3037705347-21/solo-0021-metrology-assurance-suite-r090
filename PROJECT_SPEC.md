# Metrology Assurance Suite

## Project Goal

Metrology Assurance Suite is a local HTTP service that keeps a traceable record
of laboratory measurement assurance work. It registers devices, opens a
verification case for one device, records technician measurements, requests a
repeat measurement when tolerance is exceeded, and seals the final evidence
record after review.

The steward registers a planned completion time and a priority basis when
opening a case. The service derives a deterministic measurement queue from
those attributes, exposes overdue status and queue positions on reads, and
accepts a measurement only for the case at the head of the queue.

The service is intentionally self-contained. It uses an in-memory repository
with atomic copy-on-write mutations and does not require a database or an
external service.

## Users

- Laboratory steward: registers devices and opens verification cases.
- Measurement technician: records observed values for an open case.
- Quality verifier: reviews accepted measurements and seals the case.

## Core Entities

- Device: a laboratory asset with a unique asset tag, model, room, steward, and
  lifecycle state.
- VerificationCase: one bounded assurance activity for a device, with a nominal
  value, tolerance, planned completion time, priority and priority basis,
  current state, version, and assigned steward.
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

The steward sends `POST /cases` with a device ID, nominal value, tolerance,
owner, planned completion time (`due_at`), priority (`routine`, `expedited`,
or `urgent`), and priority basis (`priority_reason`). The service verifies
that the device exists, is active, and has no unfinished case. A valid request
creates a case in `awaiting_measurement` and places it in the derived
measurement queue.

### 3. Record Measurement

A technician sends `POST /cases/{id}/measurements`. The case must be
`awaiting_measurement` and must be the current head of the measurement queue;
otherwise the request fails with `queue_head_mismatch`. The service computes
the signed deviation from the nominal value. An in-tolerance measurement moves
the case to `review_ready`; an out-of-tolerance measurement moves it to
`recheck_required`.

### 4. Reopen Verification Case

The steward sends `POST /cases/{id}/reopen` after an out-of-tolerance result.
Only `recheck_required` cases may move back to `awaiting_measurement`. The
service records the actor and reason in the audit trail, and the case rejoins
the measurement queue at its deterministic position.

### 5. Adjust Case Priority

The steward sends `POST /cases/{id}/priority` with an actor, a new priority,
and its basis. Any unfinished case may be reprioritized; a sealed case rejects
the change. The adjustment appends a `case_priority_adjusted` audit event and
updates only the current priority fields, so previously recorded history is
preserved.

### 6. Seal Verification

The verifier sends `POST /cases/{id}/seal` after an accepted measurement. Only
`review_ready` cases may be sealed. The service selects the accepted
measurement with the smallest absolute deviation, creates exactly one evidence
seal, moves the case to `sealed`, and rejects later mutations.

### 7. Read Measurement Queue

A reader sends `GET /cases/queue` to see the current processing order, or
`GET /cases/{id}` for one case's queue status. The queue is a deterministic
projection over `awaiting_measurement` cases ordered by priority rank, then
planned completion time, then opening order; it is computed at read time and
never rewrites stored history. Each entry and each case view reports whether
the case is overdue (unfinished and past its planned completion time).

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
- A measurement is accepted only for the case at the head of the derived
  measurement queue.
- A tolerance must be positive.
- A case can have at most one evidence seal.
- A sealed case cannot accept measurements, reopen, change priority, or seal
  again.
- The measurement queue is a deterministic read-time projection; new, sealed,
  and reopened cases and priority adjustments never modify recorded history.
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
- `GET /cases/queue`
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
seal. The measurement-queue check verifies the derived queue order, overdue
visibility, queue-head enforcement, priority adjustment, and that sealed and
reopened cases keep their recorded history.

Tests are intentionally deferred for this initialization baseline. The later
engineering-task stage will add unit and integration tests, including red/green
verification for the workflows and their failure paths.
