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
	DeviceID       string    `json:"device_id"`
	Nominal        float64   `json:"nominal"`
	Tolerance      float64   `json:"tolerance"`
	Steward        string    `json:"steward"`
	DueAt          time.Time `json:"due_at"`
	Priority       string    `json:"priority"`
	PriorityReason string    `json:"priority_reason"`
}

type RecordMeasurementRequest struct {
	Observed   float64 `json:"observed"`
	Technician string  `json:"technician"`
}

type ReopenCaseRequest struct {
	Actor  string `json:"actor"`
	Reason string `json:"reason"`
}

type AdjustCasePriorityRequest struct {
	Actor    string `json:"actor"`
	Priority string `json:"priority"`
	Reason   string `json:"reason"`
}

type SealCaseRequest struct {
	Reviewer string `json:"reviewer"`
	Summary  string `json:"summary"`
}

type DeviceResponse struct {
	Device domain.Device `json:"device"`
}

// QueueView is the derived schedule status of one case. Position is the
// 1-based place in the measurement queue and is present only while the case
// is awaiting measurement.
type QueueView struct {
	Overdue  bool `json:"overdue"`
	Position int  `json:"position,omitempty"`
	Size     int  `json:"size"`
}

type CaseResponse struct {
	Case         domain.VerificationCase `json:"case"`
	Measurements []domain.Measurement    `json:"measurements"`
	Seal         *domain.EvidenceSeal    `json:"seal,omitempty"`
	Events       []domain.AuditEvent     `json:"events"`
	Queue        QueueView               `json:"queue"`
}

type QueueEntry struct {
	Position int                     `json:"position"`
	Overdue  bool                    `json:"overdue"`
	Case     domain.VerificationCase `json:"case"`
}

type QueueResponse struct {
	GeneratedAt time.Time    `json:"generated_at"`
	Entries     []QueueEntry `json:"entries"`
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
