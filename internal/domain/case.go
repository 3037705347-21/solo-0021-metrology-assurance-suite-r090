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

type CasePriority string

const (
	PriorityRoutine   CasePriority = "routine"
	PriorityExpedited CasePriority = "expedited"
	PriorityUrgent    CasePriority = "urgent"
)

func ParseCasePriority(value string) (CasePriority, error) {
	switch priority := CasePriority(strings.TrimSpace(value)); priority {
	case PriorityRoutine, PriorityExpedited, PriorityUrgent:
		return priority, nil
	default:
		return "", fmt.Errorf("%w: priority must be one of routine, expedited, urgent", ErrInvalidInput)
	}
}

func (p CasePriority) Validate() error {
	switch p {
	case PriorityRoutine, PriorityExpedited, PriorityUrgent:
		return nil
	default:
		return fmt.Errorf("%w: unsupported case priority %q", ErrInvalidInput, p)
	}
}

// Rank orders priorities for the measurement queue; higher ranks are served first.
func (p CasePriority) Rank() int {
	switch p {
	case PriorityUrgent:
		return 3
	case PriorityExpedited:
		return 2
	case PriorityRoutine:
		return 1
	default:
		return 0
	}
}

type VerificationCase struct {
	ID             string       `json:"id"`
	DeviceID       string       `json:"device_id"`
	Nominal        float64      `json:"nominal"`
	Tolerance      float64      `json:"tolerance"`
	Steward        string       `json:"steward"`
	State          CaseState    `json:"state"`
	Version        int          `json:"version"`
	DueAt          time.Time    `json:"due_at"`
	Priority       CasePriority `json:"priority"`
	PriorityReason string       `json:"priority_reason"`
	OpenedAt       time.Time    `json:"opened_at"`
	UpdatedAt      time.Time    `json:"updated_at"`
	SealedAt       *time.Time   `json:"sealed_at,omitempty"`
}

func NewVerificationCase(id, deviceID string, nominal, tolerance float64, steward string, dueAt time.Time, priority CasePriority, priorityReason string, at time.Time) (VerificationCase, error) {
	item := VerificationCase{
		ID:             strings.TrimSpace(id),
		DeviceID:       strings.TrimSpace(deviceID),
		Nominal:        nominal,
		Tolerance:      tolerance,
		Steward:        strings.TrimSpace(steward),
		State:          CaseAwaitingMeasurement,
		Version:        1,
		DueAt:          dueAt.UTC(),
		Priority:       priority,
		PriorityReason: strings.TrimSpace(priorityReason),
		OpenedAt:       at.UTC(),
		UpdatedAt:      at.UTC(),
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
	if c.Version < 1 || c.OpenedAt.IsZero() || c.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: case version and timestamps are required", ErrInvalidInput)
	}
	if c.DueAt.IsZero() {
		return fmt.Errorf("%w: planned completion time is required", ErrInvalidInput)
	}
	if err := c.Priority.Validate(); err != nil {
		return err
	}
	if c.PriorityReason == "" {
		return fmt.Errorf("%w: priority reason is required", ErrInvalidInput)
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

// IsOverdue reports whether an unfinished case has passed its planned
// completion time. Sealed cases are complete and never overdue.
func (c VerificationCase) IsOverdue(at time.Time) bool {
	return c.IsUnfinished() && at.UTC().After(c.DueAt)
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

// AdjustPriority records a new priority and its basis on an unfinished case.
// The previous basis stays in the audit trail; only the current value changes.
func (c *VerificationCase) AdjustPriority(priority CasePriority, reason string, at time.Time) error {
	if c.State == CaseSealed {
		return fmt.Errorf("%w: a sealed case cannot change priority", ErrStateConflict)
	}
	if err := priority.Validate(); err != nil {
		return err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("%w: priority reason is required", ErrInvalidInput)
	}
	c.Priority = priority
	c.PriorityReason = reason
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

// MeasurementQueue projects the deterministic processing order over the cases
// that are waiting to enter measurement. The order is derived from current
// case attributes (priority, planned completion time, opening order), so
// reads never rewrite stored history: new, sealed, and reopened cases simply
// join or leave the projection.
func MeasurementQueue(cases []VerificationCase) []VerificationCase {
	queue := make([]VerificationCase, 0, len(cases))
	for _, item := range cases {
		if item.State == CaseAwaitingMeasurement {
			queue = append(queue, item)
		}
	}
	sort.Slice(queue, func(i, j int) bool {
		return queueLess(queue[i], queue[j])
	})
	return queue
}

// QueuePosition returns the 1-based position of a case in the queue.
func QueuePosition(queue []VerificationCase, caseID string) (int, bool) {
	for index, item := range queue {
		if item.ID == caseID {
			return index + 1, true
		}
	}
	return 0, false
}

func queueLess(a, b VerificationCase) bool {
	if a.Priority.Rank() != b.Priority.Rank() {
		return a.Priority.Rank() > b.Priority.Rank()
	}
	if !a.DueAt.Equal(b.DueAt) {
		return a.DueAt.Before(b.DueAt)
	}
	if !a.OpenedAt.Equal(b.OpenedAt) {
		return a.OpenedAt.Before(b.OpenedAt)
	}
	return a.ID < b.ID
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
