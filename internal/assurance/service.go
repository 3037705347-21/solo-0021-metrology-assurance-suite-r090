package assurance

import (
	"context"
	"fmt"
	"strings"
	"time"

	"example.com/solo-0021-metrology-assurance-suite/internal/contracts"
	"example.com/solo-0021-metrology-assurance-suite/internal/domain"
	"example.com/solo-0021-metrology-assurance-suite/internal/repository"
)

type Service struct {
	repository *repository.Repository
	now        func() time.Time
}

func NewService(repo *repository.Repository) *Service {
	return &Service{
		repository: repo,
		now:        time.Now,
	}
}

func (s *Service) RegisterDevice(ctx context.Context, input contracts.RegisterDeviceRequest) (domain.Device, error) {
	var created domain.Device
	tag := domain.NormalizeAssetTag(input.AssetTag)
	err := s.repository.Mutate(ctx, func(m *repository.Mutation) error {
		if _, exists := m.DeviceByTag(tag); exists {
			return fmt.Errorf("%w: %s", domain.ErrDuplicateAssetTag, tag)
		}
		item, err := domain.NewDevice(
			m.NextID("device"),
			input.AssetTag,
			input.Model,
			input.Room,
			input.Steward,
			s.now(),
		)
		if err != nil {
			return err
		}
		if err := m.PutDevice(item); err != nil {
			return err
		}
		event, err := domain.NewAuditEvent(
			m.NextID("event"),
			"device",
			item.ID,
			"",
			"device_enrolled",
			item.Steward,
			"device enrolled for assurance work",
			s.now(),
		)
		if err != nil {
			return err
		}
		m.AppendEvent(event)
		created = item
		return nil
	})
	return created, err
}

func (s *Service) Device(ctx context.Context, id string) (domain.Device, error) {
	var found domain.Device
	err := s.repository.Read(ctx, func(snapshot repository.Snapshot) error {
		item, exists := snapshot.Device(strings.TrimSpace(id))
		if !exists {
			return fmt.Errorf("%w: device %s", domain.ErrNotFound, id)
		}
		found = item
		return nil
	})
	return found, err
}

func (s *Service) OpenCase(ctx context.Context, input contracts.OpenCaseRequest) (contracts.CaseResponse, error) {
	plannedCompleteAt, err := parsePlannedCompleteAt(input.PlannedCompleteAt)
	if err != nil {
		return contracts.CaseResponse{}, err
	}
	priorityReason := strings.TrimSpace(input.PriorityReason)
	if input.Priority < domain.MinPriority || input.Priority > domain.MaxPriority || priorityReason == "" {
		return contracts.CaseResponse{}, fmt.Errorf(
			"%w: priority between %d and %d plus a priority basis are required",
			domain.ErrInvalidInput, domain.MinPriority, domain.MaxPriority,
		)
	}

	var result contracts.CaseResponse
	err = s.repository.Mutate(ctx, func(m *repository.Mutation) error {
		deviceID := strings.TrimSpace(input.DeviceID)
		device, exists := m.Device(deviceID)
		if !exists {
			return fmt.Errorf("%w: device %s", domain.ErrNotFound, deviceID)
		}
		if !device.IsActive() {
			return fmt.Errorf("%w: device %s is %s", domain.ErrStateConflict, device.ID, device.Status)
		}
		if active, exists := m.ActiveCaseForDevice(device.ID); exists {
			return fmt.Errorf("%w: case %s is still %s", domain.ErrActiveCaseExists, active.ID, active.State)
		}
		item, err := domain.NewVerificationCase(
			m.NextID("case"),
			device.ID,
			input.BatchID,
			input.Nominal,
			input.Tolerance,
			input.Steward,
			input.Priority,
			priorityReason,
			plannedCompleteAt,
			m.NextCaseSequence(),
			s.now(),
		)
		if err != nil {
			return err
		}
		if err := m.PutCase(item); err != nil {
			return err
		}
		event, err := domain.NewAuditEvent(
			m.NextID("event"),
			"case",
			item.ID,
			item.ID,
			"case_opened",
			item.Steward,
			fmt.Sprintf(
				"verification case opened for batch %q with priority %d (%s), planned completion %s",
				item.BatchID, item.Priority, item.PriorityReason,
				item.PlannedCompleteAt.Format(time.RFC3339),
			),
			s.now(),
		)
		if err != nil {
			return err
		}
		m.AppendEvent(event)
		result = decorateCase(m.AllCases(), m, item, s.now())
		return nil
	})
	return result, err
}

func (s *Service) Case(ctx context.Context, id string) (contracts.CaseResponse, error) {
	var result contracts.CaseResponse
	err := s.repository.Read(ctx, func(snapshot repository.Snapshot) error {
		item, exists := snapshot.Case(strings.TrimSpace(id))
		if !exists {
			return fmt.Errorf("%w: case %s", domain.ErrNotFound, id)
		}
		result = decorateSnapshotCase(snapshot, item, s.now())
		return nil
	})
	return result, err
}

func (s *Service) RecordMeasurement(ctx context.Context, caseID string, input contracts.RecordMeasurementRequest) (contracts.CaseResponse, error) {
	var result contracts.CaseResponse
	err := s.repository.Mutate(ctx, func(m *repository.Mutation) error {
		id := strings.TrimSpace(caseID)
		item, exists := m.Case(id)
		if !exists {
			return fmt.Errorf("%w: case %s", domain.ErrNotFound, id)
		}
		// The state rule is evaluated before the queue rule so a case that
		// has already left the bench still reports a state conflict.
		if item.State != domain.CaseAwaitingMeasurement {
			return fmt.Errorf("%w: case is not awaiting measurement", domain.ErrStateConflict)
		}
		if !isQueueHead(m.AllCases(), item.ID) {
			return fmt.Errorf("%w: only the first case in the measurement queue may enter measurement", domain.ErrNotQueueHead)
		}
		measurement, err := domain.NewMeasurement(
			m.NextID("measurement"),
			item.ID,
			input.Observed,
			item.Nominal,
			item.Tolerance,
			input.Technician,
			s.now(),
		)
		if err != nil {
			return err
		}
		if err := item.RecordMeasurement(measurement.Accepted, s.now()); err != nil {
			return err
		}
		if err := m.AppendMeasurement(measurement); err != nil {
			return err
		}
		if err := m.UpdateCase(item); err != nil {
			return err
		}
		kind := "measurement_rejected"
		detail := "measurement exceeded tolerance and requires another measurement"
		if measurement.Accepted {
			kind = "measurement_accepted"
			detail = "measurement is within tolerance and ready for review"
		}
		event, err := domain.NewAuditEvent(
			m.NextID("event"),
			"case",
			item.ID,
			item.ID,
			kind,
			measurement.Technician,
			detail,
			s.now(),
		)
		if err != nil {
			return err
		}
		m.AppendEvent(event)
		result = decorateCase(m.AllCases(), m, item, s.now())
		return nil
	})
	return result, err
}

func (s *Service) ReopenCase(ctx context.Context, caseID string, input contracts.ReopenCaseRequest) (contracts.CaseResponse, error) {
	var result contracts.CaseResponse
	err := s.repository.Mutate(ctx, func(m *repository.Mutation) error {
		id := strings.TrimSpace(caseID)
		item, exists := m.Case(id)
		if !exists {
			return fmt.Errorf("%w: case %s", domain.ErrNotFound, id)
		}
		if err := item.Reopen(s.now()); err != nil {
			return err
		}
		if err := m.UpdateCase(item); err != nil {
			return err
		}
		actor := strings.TrimSpace(input.Actor)
		reason := strings.TrimSpace(input.Reason)
		if actor == "" || reason == "" {
			return fmt.Errorf("%w: actor and reason are required", domain.ErrInvalidInput)
		}
		event, err := domain.NewAuditEvent(
			m.NextID("event"),
			"case",
			item.ID,
			item.ID,
			"case_reopened",
			actor,
			fmt.Sprintf("rework returned to measurement queue at sequence %d: %s", item.EnqueueSequence, reason),
			s.now(),
		)
		if err != nil {
			return err
		}
		m.AppendEvent(event)
		result = decorateCase(m.AllCases(), m, item, s.now())
		return nil
	})
	return result, err
}

// AdjustPriority records a steward's priority change without touching the
// case's measurements, seals, planned completion time, or enqueue sequence.
// The change is appended to the audit trail in the same atomic commit.
func (s *Service) AdjustPriority(ctx context.Context, caseID string, input contracts.AdjustPriorityRequest) (contracts.CaseResponse, error) {
	actor := strings.TrimSpace(input.Actor)
	if actor == "" {
		return contracts.CaseResponse{}, fmt.Errorf("%w: actor is required", domain.ErrInvalidInput)
	}
	var result contracts.CaseResponse
	err := s.repository.Mutate(ctx, func(m *repository.Mutation) error {
		id := strings.TrimSpace(caseID)
		item, exists := m.Case(id)
		if !exists {
			return fmt.Errorf("%w: case %s", domain.ErrNotFound, id)
		}
		previous := item.Priority
		if err := item.AdjustPriority(input.Priority, input.PriorityReason, s.now()); err != nil {
			return err
		}
		if err := m.UpdateCase(item); err != nil {
			return err
		}
		event, err := domain.NewAuditEvent(
			m.NextID("event"),
			"case",
			item.ID,
			item.ID,
			"priority_adjusted",
			actor,
			fmt.Sprintf("priority changed from %d to %d (%s)", previous, item.Priority, item.PriorityReason),
			s.now(),
		)
		if err != nil {
			return err
		}
		m.AppendEvent(event)
		result = decorateCase(m.AllCases(), m, item, s.now())
		return nil
	})
	return result, err
}

func (s *Service) SealCase(ctx context.Context, caseID string, input contracts.SealCaseRequest) (contracts.CaseResponse, error) {
	var result contracts.CaseResponse
	err := s.repository.Mutate(ctx, func(m *repository.Mutation) error {
		id := strings.TrimSpace(caseID)
		item, exists := m.Case(id)
		if !exists {
			return fmt.Errorf("%w: case %s", domain.ErrNotFound, id)
		}
		if _, exists := m.Seal(item.ID); exists {
			return fmt.Errorf("%w: case %s", domain.ErrSealAlreadyCreated, item.ID)
		}
		measurement, ok := bestAcceptedMeasurement(m.Measurements(item.ID))
		if !ok {
			return fmt.Errorf("%w: an accepted measurement is required before sealing", domain.ErrStateConflict)
		}
		if err := item.Seal(s.now()); err != nil {
			return err
		}
		seal, err := domain.NewEvidenceSeal(
			m.NextID("seal"),
			item.ID,
			item.DeviceID,
			measurement.ID,
			input.Reviewer,
			input.Summary,
			s.now(),
		)
		if err != nil {
			return err
		}
		if err := m.PutSeal(seal); err != nil {
			return err
		}
		if err := m.UpdateCase(item); err != nil {
			return err
		}
		event, err := domain.NewAuditEvent(
			m.NextID("event"),
			"case",
			item.ID,
			item.ID,
			"case_sealed",
			seal.Reviewer,
			seal.Summary,
			s.now(),
		)
		if err != nil {
			return err
		}
		m.AppendEvent(event)
		result = decorateCase(m.AllCases(), m, item, s.now())
		return nil
	})
	return result, err
}

// Queue reads the current measurement queue. Without filters it returns the
// single global queue whose first entry is the only case allowed into
// measurement. Filtering by device or batch narrows the listing but keeps
// global positions, so a filtered view can never suggest a different head
// than the enforcement rule.
func (s *Service) Queue(ctx context.Context, deviceID, batchID string) (contracts.QueueResponse, error) {
	deviceID = strings.TrimSpace(deviceID)
	batchID = strings.TrimSpace(batchID)
	var response contracts.QueueResponse
	err := s.repository.Read(ctx, func(snapshot repository.Snapshot) error {
		if deviceID != "" {
			if _, exists := snapshot.Device(deviceID); !exists {
				return fmt.Errorf("%w: device %s", domain.ErrNotFound, deviceID)
			}
		}
		now := s.now()
		ordered := domain.QueuedCases(snapshot.AllCases())
		response = contracts.QueueResponse{
			GeneratedAt: now.UTC(),
			DeviceID:    deviceID,
			BatchID:     batchID,
			Queue:       make([]contracts.QueueEntry, 0),
		}
		for position, item := range ordered {
			if deviceID != "" && item.DeviceID != deviceID {
				continue
			}
			if batchID != "" && item.BatchID != batchID {
				continue
			}
			response.Queue = append(response.Queue, contracts.QueueEntry{
				Position:          position + 1,
				CaseID:            item.ID,
				DeviceID:          item.DeviceID,
				BatchID:           item.BatchID,
				State:             string(item.State),
				Priority:          item.Priority,
				PriorityReason:    item.PriorityReason,
				Steward:           item.Steward,
				PlannedCompleteAt: item.PlannedCompleteAt,
				EnqueueSequence:   item.EnqueueSequence,
				Overdue:           item.IsOverdue(now),
			})
		}
		return nil
	})
	return response, err
}

func parsePlannedCompleteAt(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, fmt.Errorf("%w: planned completion time is required", domain.ErrInvalidInput)
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: planned completion time must be RFC3339: %v", domain.ErrInvalidInput, err)
	}
	return parsed.UTC(), nil
}

func bestAcceptedMeasurement(items []domain.Measurement) (domain.Measurement, bool) {
	var selected domain.Measurement
	found := false
	for _, item := range items {
		if !item.Accepted {
			continue
		}
		if !found || item.AbsoluteDeviation() < selected.AbsoluteDeviation() {
			selected = item
			found = true
		}
	}
	return selected, found
}

// isQueueHead and queuePositionOf operate on the same deterministic ordering
// produced by domain.QueuedCases, so the read view and the measurement gate
// can never disagree.
func isQueueHead(items []domain.VerificationCase, caseID string) bool {
	ordered := domain.QueuedCases(items)
	return len(ordered) > 0 && ordered[0].ID == caseID
}

func queuePositionOf(items []domain.VerificationCase, caseID string) *int {
	for position, item := range domain.QueuedCases(items) {
		if item.ID == caseID {
			index := position + 1
			return &index
		}
	}
	return nil
}

func decorateCase(items []domain.VerificationCase, m *repository.Mutation, item domain.VerificationCase, now time.Time) contracts.CaseResponse {
	result := contracts.CaseResponse{
		Case:          item,
		Measurements:  m.Measurements(item.ID),
		Events:        m.Events(item.ID),
		QueuePosition: queuePositionOf(items, item.ID),
		Overdue:       item.IsOverdue(now),
	}
	if seal, exists := m.Seal(item.ID); exists {
		result.Seal = &seal
	}
	return result
}

func decorateSnapshotCase(snapshot repository.Snapshot, item domain.VerificationCase, now time.Time) contracts.CaseResponse {
	result := contracts.CaseResponse{
		Case:          item,
		Measurements:  snapshot.Measurements(item.ID),
		Events:        snapshot.Events(item.ID),
		QueuePosition: queuePositionOf(snapshot.AllCases(), item.ID),
		Overdue:       item.IsOverdue(now),
	}
	if seal, exists := snapshot.Seal(item.ID); exists {
		result.Seal = &seal
	}
	return result
}
