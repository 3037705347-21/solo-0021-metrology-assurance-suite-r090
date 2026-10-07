package contracts

import (
	"time"

	"example.com/solo-0021-metrology-assurance-suite/internal/domain"
)

type RegisterDeviceRequest struct {
	AssetTag string `json:"asset_tag"`
	Model    string `json:"model"`
	Room     string `json:"room"`
	Steward  string `json:"steward"`
}

type OpenCaseRequest struct {
	DeviceID          string  `json:"device_id"`
	BatchID           string  `json:"batch_id"`
	Nominal           float64 `json:"nominal"`
	Tolerance         float64 `json:"tolerance"`
	Steward           string  `json:"steward"`
	Priority          int     `json:"priority"`
	PriorityReason    string  `json:"priority_reason"`
	PlannedCompleteAt string  `json:"planned_complete_at"`
}

type AdjustPriorityRequest struct {
	Priority       int    `json:"priority"`
	PriorityReason string `json:"priority_reason"`
	Actor          string `json:"actor"`
}

type RecordMeasurementRequest struct {
	Observed   float64 `json:"observed"`
	Technician string  `json:"technician"`
}

type ReopenCaseRequest struct {
	Actor  string `json:"actor"`
	Reason string `json:"reason"`
}

type SealCaseRequest struct {
	Reviewer string `json:"reviewer"`
	Summary  string `json:"summary"`
}

type DeviceResponse struct {
	Device domain.Device `json:"device"`
}

type CaseResponse struct {
	Case         domain.VerificationCase `json:"case"`
	Measurements []domain.Measurement    `json:"measurements"`
	Seal         *domain.EvidenceSeal    `json:"seal,omitempty"`
	Events       []domain.AuditEvent     `json:"events"`
	// QueuePosition is the 1-based position in the current measurement queue
	// and is null whenever the case is not awaiting measurement.
	QueuePosition *int `json:"queue_position"`
	// Overdue reports an unfinished case past its promised completion time.
	Overdue bool `json:"overdue"`
}

// QueueEntry is one queued case as seen by the lab floor. Positions are
// always expressed against the single global queue, even when the view was
// narrowed by device or batch, so the first entry shown and the case allowed
// into measurement can never disagree.
type QueueEntry struct {
	Position          int       `json:"position"`
	CaseID            string    `json:"case_id"`
	DeviceID          string    `json:"device_id"`
	BatchID           string    `json:"batch_id,omitempty"`
	State             string    `json:"state"`
	Priority          int       `json:"priority"`
	PriorityReason    string    `json:"priority_reason"`
	Steward           string    `json:"steward"`
	PlannedCompleteAt time.Time `json:"planned_complete_at"`
	EnqueueSequence   int64     `json:"enqueue_sequence"`
	Overdue           bool      `json:"overdue"`
}

type QueueResponse struct {
	GeneratedAt time.Time    `json:"generated_at"`
	DeviceID    string       `json:"device_id,omitempty"`
	BatchID     string       `json:"batch_id,omitempty"`
	Queue       []QueueEntry `json:"queue"`
}

type EventsResponse struct {
	Events []domain.AuditEvent `json:"events"`
}

type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
