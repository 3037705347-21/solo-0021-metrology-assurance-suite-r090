package domain

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

type CaseState string

const (
	CaseAwaitingMeasurement CaseState = "awaiting_measurement"
	CaseReviewReady         CaseState = "review_ready"
	CaseRecheckRequired     CaseState = "recheck_required"
	CaseSealed              CaseState = "sealed"
)

// Priority is a 1..5 rank where 1 is the most urgent. The rank is recorded
// together with the steward's priority basis so the queue order stays
// explainable.
const (
	MinPriority = 1
	MaxPriority = 5
)

type VerificationCase struct {
	ID                string     `json:"id"`
	DeviceID          string     `json:"device_id"`
	BatchID           string     `json:"batch_id,omitempty"`
	Nominal           float64    `json:"nominal"`
	Tolerance         float64    `json:"tolerance"`
	Steward           string     `json:"steward"`
	State             CaseState  `json:"state"`
	Version           int        `json:"version"`
	Priority          int        `json:"priority"`
	PriorityReason    string     `json:"priority_reason"`
	PlannedCompleteAt time.Time  `json:"planned_complete_at"`
	EnqueueSequence   int64      `json:"enqueue_sequence"`
	OpenedAt          time.Time  `json:"opened_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	SealedAt          *time.Time `json:"sealed_at,omitempty"`
}

func NewVerificationCase(
	id, deviceID, batchID string,
	nominal, tolerance float64,
	steward string,
	priority int,
	priorityReason string,
	plannedCompleteAt time.Time,
	enqueueSequence int64,
	at time.Time,
) (VerificationCase, error) {
	item := VerificationCase{
		ID:                strings.TrimSpace(id),
		DeviceID:          strings.TrimSpace(deviceID),
		BatchID:           strings.TrimSpace(batchID),
		Nominal:           nominal,
		Tolerance:         tolerance,
		Steward:           strings.TrimSpace(steward),
		State:             CaseAwaitingMeasurement,
		Version:           1,
		Priority:          priority,
		PriorityReason:    strings.TrimSpace(priorityReason),
		PlannedCompleteAt: plannedCompleteAt.UTC(),
		EnqueueSequence:   enqueueSequence,
		OpenedAt:          at.UTC(),
		UpdatedAt:         at.UTC(),
	}
	if err := item.Validate(); err != nil {
		return VerificationCase{}, err
	}
	return item, nil
}

func (c VerificationCase) Validate() error {
	if c.ID == "" || c.DeviceID == "" || c.Steward == "" {
		return fmt.Errorf("%w: case id, device id, and steward are required", ErrInvalidInput)
	}
	if !isFinite(c.Nominal) || !isFinite(c.Tolerance) || c.Tolerance <= 0 {
		return fmt.Errorf("%w: nominal must be finite and tolerance must be positive", ErrInvalidInput)
	}
	if c.Priority < MinPriority || c.Priority > MaxPriority {
		return fmt.Errorf("%w: priority must be between %d and %d", ErrInvalidInput, MinPriority, MaxPriority)
	}
	if c.PriorityReason == "" {
		return fmt.Errorf("%w: priority basis is required", ErrInvalidInput)
	}
	if len(c.BatchID) > 64 || len(c.PriorityReason) > 512 {
		return fmt.Errorf("%w: batch id or priority basis is too long", ErrInvalidInput)
	}
	if c.PlannedCompleteAt.IsZero() {
		return fmt.Errorf("%w: planned completion time is required", ErrInvalidInput)
	}
	if c.EnqueueSequence <= 0 {
		return fmt.Errorf("%w: enqueue sequence is required", ErrInvalidInput)
	}
	if c.Version < 1 || c.OpenedAt.IsZero() || c.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: case version and timestamps are required", ErrInvalidInput)
	}
	switch c.State {
	case CaseAwaitingMeasurement, CaseReviewReady, CaseRecheckRequired, CaseSealed:
		return nil
	default:
		return fmt.Errorf("%w: unsupported case state %q", ErrInvalidInput, c.State)
	}
}

func (c VerificationCase) IsUnfinished() bool {
	return c.State != CaseSealed
}

// IsQueued reports whether the case currently occupies the measurement queue.
// Only cases actively awaiting a measurement block the bench; review-ready and
// recheck-required cases have left the queue until they are reopened.
func (c VerificationCase) IsQueued() bool {
	return c.State == CaseAwaitingMeasurement
}

// IsOverdue reports an unfinished case whose promised completion time has
// passed at the given instant. Sealed history is never reported as overdue.
func (c VerificationCase) IsOverdue(at time.Time) bool {
	return c.IsUnfinished() && at.UTC().After(c.PlannedCompleteAt)
}

// AdjustPriority records a new priority rank together with the steward's
// basis for the change. Sealed cases are historical and cannot move. The
// enqueue sequence is deliberately untouched: re-prioritizing a case never
// rewrites its place identity, it only changes how the queue is ordered.
func (c *VerificationCase) AdjustPriority(priority int, reason string, at time.Time) error {
	if c.State == CaseSealed {
		return fmt.Errorf("%w: a sealed case cannot be reprioritized", ErrStateConflict)
	}
	if priority < MinPriority || priority > MaxPriority {
		return fmt.Errorf("%w: priority must be between %d and %d", ErrInvalidInput, MinPriority, MaxPriority)
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("%w: priority basis is required", ErrInvalidInput)
	}
	c.Priority = priority
	c.PriorityReason = reason
	c.Version++
	c.UpdatedAt = at.UTC()
	return nil
}

// QueuedCases returns the cases currently waiting for measurement in the
// stable processing order: higher urgency (lower rank) first, then the
// earlier promised completion time, then the immutable enqueue sequence
// assigned when the case was first opened. A reworked case keeps its original
// sequence, so returning from recheck never rewrites history.
func QueuedCases(items []VerificationCase) []VerificationCase {
	queued := make([]VerificationCase, 0, len(items))
	for _, item := range items {
		if item.IsQueued() {
			queued = append(queued, item)
		}
	}
	sort.SliceStable(queued, func(i, j int) bool {
		if queued[i].Priority != queued[j].Priority {
			return queued[i].Priority < queued[j].Priority
		}
		if !queued[i].PlannedCompleteAt.Equal(queued[j].PlannedCompleteAt) {
			return queued[i].PlannedCompleteAt.Before(queued[j].PlannedCompleteAt)
		}
		return queued[i].EnqueueSequence < queued[j].EnqueueSequence
	})
	return queued
}

func (c *VerificationCase) RecordMeasurement(accepted bool, at time.Time) error {
	if c.State != CaseAwaitingMeasurement {
		return fmt.Errorf("%w: case is not awaiting measurement", ErrStateConflict)
	}
	if accepted {
		c.State = CaseReviewReady
	} else {
		c.State = CaseRecheckRequired
	}
	c.Version++
	c.UpdatedAt = at.UTC()
	return nil
}

func (c *VerificationCase) Reopen(at time.Time) error {
	if c.State != CaseRecheckRequired {
		return fmt.Errorf("%w: only a recheck_required case can be reopened", ErrStateConflict)
	}
	c.State = CaseAwaitingMeasurement
	c.Version++
	c.UpdatedAt = at.UTC()
	return nil
}

func (c *VerificationCase) Seal(at time.Time) error {
	if c.State != CaseReviewReady {
		return fmt.Errorf("%w: case is not ready for review", ErrStateConflict)
	}
	stamp := at.UTC()
	c.State = CaseSealed
	c.Version++
	c.UpdatedAt = stamp
	c.SealedAt = &stamp
	return nil
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
