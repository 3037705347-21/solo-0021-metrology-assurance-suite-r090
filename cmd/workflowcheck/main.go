package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"time"

	"example.com/solo-0021-metrology-assurance-suite/internal/bootstrap"
)

type apiResponse struct {
	Status int
	Body   map[string]any
	Raw    string
}

type harness struct {
	baseURL string
	client  *http.Client
	server  *httptest.Server
}

func main() {
	workflow := flag.String("workflow", "", "workflow id to verify")
	flag.Parse()

	checks := map[string]func() error{
		"register-device":          checkRegisterDevice,
		"open-verification-case":   checkOpenVerificationCase,
		"record-measurement":       checkRecordMeasurement,
		"reopen-verification-case": checkReopenVerificationCase,
		"seal-verification":        checkSealVerification,
		"measurement-queue":        checkMeasurementQueue,
	}
	run, ok := checks[*workflow]
	if !ok {
		fmt.Fprintf(flag.CommandLine.Output(), "unknown workflow %q\n", *workflow)
		flag.Usage()
		os.Exit(2)
	}
	if err := run(); err != nil {
		fmt.Printf("workflow %s: failed: %v\n", *workflow, err)
		os.Exit(1)
	}
	fmt.Printf("workflow %s: ok\n", *workflow)
}

func checkRegisterDevice() error {
	h := newHarness()
	defer h.close()

	response, err := h.request(http.MethodPost, "/devices", map[string]any{
		"asset_tag": " met-1001 ",
		"model":     "Bench Meter",
		"room":      "Room A",
		"steward":   "Lin",
	})
	if err != nil {
		return err
	}
	if err := expectStatus(response, http.StatusCreated); err != nil {
		return err
	}
	id, err := nestedString(response.Body, "device", "id")
	if err != nil {
		return err
	}
	tag, err := nestedString(response.Body, "device", "asset_tag")
	if err != nil {
		return err
	}
	if tag != "MET-1001" {
		return fmt.Errorf("asset tag was not normalized: %q", tag)
	}

	read, err := h.request(http.MethodGet, "/devices/"+id, nil)
	if err != nil {
		return err
	}
	if err := expectStatus(read, http.StatusOK); err != nil {
		return err
	}
	readID, err := nestedString(read.Body, "device", "id")
	if err != nil {
		return err
	}
	if readID != id {
		return fmt.Errorf("read returned device %q, expected %q", readID, id)
	}

	duplicate, err := h.request(http.MethodPost, "/devices", map[string]any{
		"asset_tag": "MET-1001",
		"model":     "Other Meter",
		"room":      "Room B",
		"steward":   "Mira",
	})
	if err != nil {
		return err
	}
	if err := expectStatus(duplicate, http.StatusConflict); err != nil {
		return err
	}
	return expectErrorCode(duplicate, "duplicate_asset_tag")
}

func checkOpenVerificationCase() error {
	h := newHarness()
	defer h.close()

	deviceID, err := h.createDevice("MET-1101")
	if err != nil {
		return err
	}
	opened, err := h.openCase(deviceID)
	if err != nil {
		return err
	}
	caseID, err := nestedString(opened.Body, "case", "id")
	if err != nil {
		return err
	}
	state, err := nestedString(opened.Body, "case", "state")
	if err != nil {
		return err
	}
	if state != "awaiting_measurement" {
		return fmt.Errorf("opened case state = %q", state)
	}
	dueAt, err := nestedString(opened.Body, "case", "due_at")
	if err != nil {
		return err
	}
	if dueAt == "" {
		return errors.New("opened case has no planned completion time")
	}
	priority, err := nestedString(opened.Body, "case", "priority")
	if err != nil {
		return err
	}
	if priority != "routine" {
		return fmt.Errorf("opened case priority = %q", priority)
	}

	read, err := h.request(http.MethodGet, "/cases/"+caseID, nil)
	if err != nil {
		return err
	}
	if err := expectStatus(read, http.StatusOK); err != nil {
		return err
	}

	second, err := h.request(http.MethodPost, "/cases", map[string]any{
		"device_id":       deviceID,
		"nominal":         25.0,
		"tolerance":       0.5,
		"steward":         "Qiao",
		"due_at":          time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339),
		"priority":        "routine",
		"priority_reason": "standard verification plan",
	})
	if err != nil {
		return err
	}
	if err := expectStatus(second, http.StatusConflict); err != nil {
		return err
	}
	return expectErrorCode(second, "active_case_exists")
}

func checkRecordMeasurement() error {
	h := newHarness()
	defer h.close()

	deviceID, err := h.createDevice("MET-1201")
	if err != nil {
		return err
	}
	opened, err := h.openCase(deviceID)
	if err != nil {
		return err
	}
	caseID, err := nestedString(opened.Body, "case", "id")
	if err != nil {
		return err
	}
	accepted, err := h.recordMeasurement(caseID, 25.2, "Rui")
	if err != nil {
		return err
	}
	if err := expectStatus(accepted, http.StatusCreated); err != nil {
		return err
	}
	state, err := nestedString(accepted.Body, "case", "state")
	if err != nil {
		return err
	}
	if state != "review_ready" {
		return fmt.Errorf("accepted measurement state = %q", state)
	}
	acceptedValue, err := nestedBool(accepted.Body, "measurements", 0, "accepted")
	if err != nil {
		return err
	}
	if !acceptedValue {
		return errors.New("in-tolerance measurement was not accepted")
	}

	second, err := h.recordMeasurement(caseID, 25.1, "Rui")
	if err != nil {
		return err
	}
	if err := expectStatus(second, http.StatusConflict); err != nil {
		return err
	}
	if err := expectErrorCode(second, "state_conflict"); err != nil {
		return err
	}

	rejectDeviceID, err := h.createDevice("MET-1202")
	if err != nil {
		return err
	}
	rejectCase, err := h.openCase(rejectDeviceID)
	if err != nil {
		return err
	}
	rejectCaseID, err := nestedString(rejectCase.Body, "case", "id")
	if err != nil {
		return err
	}
	rejected, err := h.recordMeasurement(rejectCaseID, 26.5, "Rui")
	if err != nil {
		return err
	}
	if err := expectStatus(rejected, http.StatusCreated); err != nil {
		return err
	}
	rejectedState, err := nestedString(rejected.Body, "case", "state")
	if err != nil {
		return err
	}
	if rejectedState != "recheck_required" {
		return fmt.Errorf("out-of-tolerance state = %q", rejectedState)
	}
	rejectedValue, err := nestedBool(rejected.Body, "measurements", 0, "accepted")
	if err != nil {
		return err
	}
	if rejectedValue {
		return errors.New("out-of-tolerance measurement was accepted")
	}
	return nil
}

func checkReopenVerificationCase() error {
	h := newHarness()
	defer h.close()

	caseID, err := h.rejectedCase("MET-1301")
	if err != nil {
		return err
	}
	reopened, err := h.request(http.MethodPost, "/cases/"+caseID+"/reopen", map[string]any{
		"actor":  "Lin",
		"reason": "measurement setup was replaced",
	})
	if err != nil {
		return err
	}
	if err := expectStatus(reopened, http.StatusOK); err != nil {
		return err
	}
	state, err := nestedString(reopened.Body, "case", "state")
	if err != nil {
		return err
	}
	if state != "awaiting_measurement" {
		return fmt.Errorf("reopened case state = %q", state)
	}
	measurements, err := nestedSlice(reopened.Body, "measurements")
	if err != nil {
		return err
	}
	if len(measurements) != 1 {
		return fmt.Errorf("reopen changed measurement history: %d entries", len(measurements))
	}

	second, err := h.request(http.MethodPost, "/cases/"+caseID+"/reopen", map[string]any{
		"actor":  "Lin",
		"reason": "duplicate request",
	})
	if err != nil {
		return err
	}
	if err := expectStatus(second, http.StatusConflict); err != nil {
		return err
	}
	if err := expectErrorCode(second, "state_conflict"); err != nil {
		return err
	}

	events, err := h.request(http.MethodGet, "/cases/"+caseID+"/events", nil)
	if err != nil {
		return err
	}
	if err := expectStatus(events, http.StatusOK); err != nil {
		return err
	}
	if !hasEvent(events.Body, "case_reopened") {
		return errors.New("reopen audit event is missing")
	}
	return nil
}

func checkSealVerification() error {
	h := newHarness()
	defer h.close()

	caseID, err := h.readyCase("MET-1401")
	if err != nil {
		return err
	}
	type outcome struct {
		response apiResponse
		err      error
	}
	results := make(chan outcome, 2)
	var wait sync.WaitGroup
	for index := 0; index < 2; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			response, err := h.request(http.MethodPost, "/cases/"+caseID+"/seal", map[string]any{
				"reviewer": fmt.Sprintf("Reviewer-%d", index+1),
				"summary":  "accepted measurement sealed for review",
			})
			results <- outcome{response: response, err: err}
		}(index)
	}
	wait.Wait()
	close(results)

	successes := 0
	conflicts := 0
	for item := range results {
		if item.err != nil {
			return item.err
		}
		switch item.response.Status {
		case http.StatusCreated:
			successes++
		case http.StatusConflict:
			conflicts++
		default:
			return fmt.Errorf("unexpected concurrent seal status %d: %s", item.response.Status, item.response.Raw)
		}
	}
	if successes != 1 || conflicts != 1 {
		return fmt.Errorf("concurrent seal outcomes: %d success, %d conflict", successes, conflicts)
	}

	read, err := h.request(http.MethodGet, "/cases/"+caseID, nil)
	if err != nil {
		return err
	}
	if err := expectStatus(read, http.StatusOK); err != nil {
		return err
	}
	state, err := nestedString(read.Body, "case", "state")
	if err != nil {
		return err
	}
	if state != "sealed" {
		return fmt.Errorf("sealed case state = %q", state)
	}
	sealID, err := nestedString(read.Body, "seal", "id")
	if err != nil {
		return err
	}
	if sealID == "" {
		return errors.New("evidence seal is missing")
	}
	return nil
}

func checkMeasurementQueue() error {
	h := newHarness()
	defer h.close()

	// Case A: routine priority, due in 24h. Case B: urgent, due in 48h.
	deviceA, err := h.createDevice("MET-1501")
	if err != nil {
		return err
	}
	dueA := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	caseA, err := h.openCaseWithSchedule(deviceA, dueA, "routine", "routine calibration round")
	if err != nil {
		return err
	}
	deviceB, err := h.createDevice("MET-1502")
	if err != nil {
		return err
	}
	caseB, err := h.openCaseWithSchedule(deviceB, time.Now().UTC().Add(48*time.Hour), "urgent", "customer audit deadline")
	if err != nil {
		return err
	}

	// The urgent case leads the queue even though it was opened second.
	if err := h.expectQueueOrder(caseB, caseA); err != nil {
		return err
	}

	// Only the queue head may enter measurement.
	blocked, err := h.recordMeasurement(caseA, 25.1, "Rui")
	if err != nil {
		return err
	}
	if err := expectStatus(blocked, http.StatusConflict); err != nil {
		return err
	}
	if err := expectErrorCode(blocked, "queue_head_mismatch"); err != nil {
		return err
	}

	readB, err := h.request(http.MethodGet, "/cases/"+caseB, nil)
	if err != nil {
		return err
	}
	if err := expectStatus(readB, http.StatusOK); err != nil {
		return err
	}
	if err := expectQueueView(readB.Body, 1, 2, false); err != nil {
		return fmt.Errorf("case B queue view: %w", err)
	}

	// Case C is registered with an already-past planned completion time.
	deviceC, err := h.createDevice("MET-1503")
	if err != nil {
		return err
	}
	caseC, err := h.openCaseWithSchedule(deviceC, time.Now().UTC().Add(-1*time.Hour), "routine", "late intake")
	if err != nil {
		return err
	}
	readC, err := h.request(http.MethodGet, "/cases/"+caseC, nil)
	if err != nil {
		return err
	}
	if err := expectStatus(readC, http.StatusOK); err != nil {
		return err
	}
	if err := expectQueueView(readC.Body, 2, 3, true); err != nil {
		return fmt.Errorf("case C queue view: %w", err)
	}

	// Raising case A to urgent makes it the head because its planned
	// completion time is earlier than case B's.
	adjusted, err := h.request(http.MethodPost, "/cases/"+caseA+"/priority", map[string]any{
		"actor":    "Lin",
		"priority": "urgent",
		"reason":   "customer moved audit forward",
	})
	if err != nil {
		return err
	}
	if err := expectStatus(adjusted, http.StatusOK); err != nil {
		return err
	}
	priority, err := nestedString(adjusted.Body, "case", "priority")
	if err != nil {
		return err
	}
	if priority != "urgent" {
		return fmt.Errorf("adjusted priority = %q", priority)
	}
	reason, err := nestedString(adjusted.Body, "case", "priority_reason")
	if err != nil {
		return err
	}
	if reason != "customer moved audit forward" {
		return fmt.Errorf("adjusted priority reason = %q", reason)
	}
	version, err := nestedFloat(adjusted.Body, "case", "version")
	if err != nil {
		return err
	}
	if version != 2 {
		return fmt.Errorf("adjusted case version = %v, expected 2", version)
	}
	// The adjustment must not rewrite the registered planned completion time.
	dueAt, err := nestedString(adjusted.Body, "case", "due_at")
	if err != nil {
		return err
	}
	if dueAt != dueA.Format(time.RFC3339) {
		return fmt.Errorf("adjustment changed planned completion time: %q", dueAt)
	}
	if !hasEvent(adjusted.Body, "case_opened") || !hasEvent(adjusted.Body, "case_priority_adjusted") {
		return errors.New("priority adjustment did not preserve the audit trail")
	}
	if err := h.expectQueueOrder(caseA, caseB, caseC); err != nil {
		return err
	}

	// Measuring the head removes it from the queue; sealing keeps history.
	measuredA, err := h.recordMeasurement(caseA, 25.1, "Rui")
	if err != nil {
		return err
	}
	if err := expectStatus(measuredA, http.StatusCreated); err != nil {
		return err
	}
	if err := h.expectQueueOrder(caseB, caseC); err != nil {
		return err
	}
	sealedA, err := h.request(http.MethodPost, "/cases/"+caseA+"/seal", map[string]any{
		"reviewer": "Reviewer-1",
		"summary":  "accepted measurement sealed for review",
	})
	if err != nil {
		return err
	}
	if err := expectStatus(sealedA, http.StatusCreated); err != nil {
		return err
	}
	if err := expectQueueView(sealedA.Body, 0, 2, false); err != nil {
		return fmt.Errorf("sealed case A queue view: %w", err)
	}
	sealedAdjust, err := h.request(http.MethodPost, "/cases/"+caseA+"/priority", map[string]any{
		"actor":    "Lin",
		"priority": "routine",
		"reason":   "attempt to rewrite a sealed case",
	})
	if err != nil {
		return err
	}
	if err := expectStatus(sealedAdjust, http.StatusConflict); err != nil {
		return err
	}
	if err := expectErrorCode(sealedAdjust, "state_conflict"); err != nil {
		return err
	}

	// A rejected measurement takes case B out of the queue; reopening brings
	// it back at the deterministic position without erasing the failed record.
	rejectedB, err := h.recordMeasurement(caseB, 26.5, "Rui")
	if err != nil {
		return err
	}
	if err := expectStatus(rejectedB, http.StatusCreated); err != nil {
		return err
	}
	if err := h.expectQueueOrder(caseC); err != nil {
		return err
	}
	reopenedB, err := h.request(http.MethodPost, "/cases/"+caseB+"/reopen", map[string]any{
		"actor":  "Lin",
		"reason": "measurement setup was replaced",
	})
	if err != nil {
		return err
	}
	if err := expectStatus(reopenedB, http.StatusOK); err != nil {
		return err
	}
	measurementsB, err := nestedSlice(reopenedB.Body, "measurements")
	if err != nil {
		return err
	}
	if len(measurementsB) != 1 {
		return fmt.Errorf("reopen changed measurement history: %d entries", len(measurementsB))
	}
	if err := h.expectQueueOrder(caseB, caseC); err != nil {
		return err
	}

	// Both remaining cases are measured in queue order; the queue drains.
	acceptedB, err := h.recordMeasurement(caseB, 25.2, "Rui")
	if err != nil {
		return err
	}
	if err := expectStatus(acceptedB, http.StatusCreated); err != nil {
		return err
	}
	acceptedC, err := h.recordMeasurement(caseC, 25.1, "Rui")
	if err != nil {
		return err
	}
	if err := expectStatus(acceptedC, http.StatusCreated); err != nil {
		return err
	}
	return h.expectQueueOrder()
}

func (h *harness) expectQueueOrder(expected ...string) error {
	response, err := h.request(http.MethodGet, "/cases/queue", nil)
	if err != nil {
		return err
	}
	if err := expectStatus(response, http.StatusOK); err != nil {
		return err
	}
	entries, err := nestedSlice(response.Body, "entries")
	if err != nil {
		return err
	}
	if len(entries) != len(expected) {
		return fmt.Errorf("queue has %d entries, expected %d: %s", len(entries), len(expected), response.Raw)
	}
	for index, caseID := range expected {
		position, err := nestedFloat(entries[index].(map[string]any), "position")
		if err != nil {
			return err
		}
		if position != float64(index+1) {
			return fmt.Errorf("queue entry %d position = %v", index, position)
		}
		id, err := nestedString(entries[index].(map[string]any), "case", "id")
		if err != nil {
			return err
		}
		if id != caseID {
			return fmt.Errorf("queue entry %d = %q, expected %q", index, id, caseID)
		}
	}
	return nil
}

func expectQueueView(body map[string]any, position, size int, overdue bool) error {
	value, err := nestedBool(body, "queue", "overdue")
	if err != nil {
		return err
	}
	if value != overdue {
		return fmt.Errorf("overdue = %v, expected %v", value, overdue)
	}
	actualSize, err := nestedFloat(body, "queue", "size")
	if err != nil {
		return err
	}
	if actualSize != float64(size) {
		return fmt.Errorf("queue size = %v, expected %d", actualSize, size)
	}
	actualPosition, err := nestedFloat(body, "queue", "position")
	if position == 0 {
		if err == nil {
			return fmt.Errorf("queue position = %v, expected none", actualPosition)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if actualPosition != float64(position) {
		return fmt.Errorf("queue position = %v, expected %d", actualPosition, position)
	}
	return nil
}

func newHarness() *harness {
	server := httptest.NewServer(bootstrap.NewHandler())
	return &harness{
		baseURL: server.URL,
		client:  server.Client(),
		server:  server,
	}
}

func (h *harness) close() {
	h.server.Close()
}

func (h *harness) request(method, path string, body any) (apiResponse, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return apiResponse{}, err
		}
		reader = bytes.NewReader(encoded)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, h.baseURL+path, reader)
	if err != nil {
		return apiResponse{}, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := h.client.Do(request)
	if err != nil {
		return apiResponse{}, err
	}
	defer response.Body.Close()
	rawBytes, err := io.ReadAll(response.Body)
	if err != nil {
		return apiResponse{}, err
	}
	result := apiResponse{Status: response.StatusCode, Raw: string(rawBytes)}
	if len(bytes.TrimSpace(rawBytes)) > 0 {
		if err := json.Unmarshal(rawBytes, &result.Body); err != nil {
			return apiResponse{}, fmt.Errorf("decode %s %s response: %w", method, path, err)
		}
	}
	return result, nil
}

func (h *harness) createDevice(tag string) (string, error) {
	response, err := h.request(http.MethodPost, "/devices", map[string]any{
		"asset_tag": tag,
		"model":     "Bench Meter",
		"room":      "Room A",
		"steward":   "Lin",
	})
	if err != nil {
		return "", err
	}
	if err := expectStatus(response, http.StatusCreated); err != nil {
		return "", err
	}
	return nestedString(response.Body, "device", "id")
}

func (h *harness) openCase(deviceID string) (apiResponse, error) {
	return h.openCaseWith(deviceID, map[string]any{
		"due_at":          time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339),
		"priority":        "routine",
		"priority_reason": "standard verification plan",
	})
}

func (h *harness) openCaseWithSchedule(deviceID string, dueAt time.Time, priority, reason string) (string, error) {
	response, err := h.openCaseWith(deviceID, map[string]any{
		"due_at":          dueAt.UTC().Format(time.RFC3339),
		"priority":        priority,
		"priority_reason": reason,
	})
	if err != nil {
		return "", err
	}
	return nestedString(response.Body, "case", "id")
}

func (h *harness) openCaseWith(deviceID string, schedule map[string]any) (apiResponse, error) {
	body := map[string]any{
		"device_id": deviceID,
		"nominal":   25.0,
		"tolerance": 0.5,
		"steward":   "Qiao",
	}
	for key, value := range schedule {
		body[key] = value
	}
	response, err := h.request(http.MethodPost, "/cases", body)
	if err != nil {
		return apiResponse{}, err
	}
	if err := expectStatus(response, http.StatusCreated); err != nil {
		return apiResponse{}, err
	}
	return response, nil
}

func (h *harness) recordMeasurement(caseID string, observed float64, technician string) (apiResponse, error) {
	return h.request(http.MethodPost, "/cases/"+caseID+"/measurements", map[string]any{
		"observed":   observed,
		"technician": technician,
	})
}

func (h *harness) rejectedCase(tag string) (string, error) {
	deviceID, err := h.createDevice(tag)
	if err != nil {
		return "", err
	}
	opened, err := h.openCase(deviceID)
	if err != nil {
		return "", err
	}
	caseID, err := nestedString(opened.Body, "case", "id")
	if err != nil {
		return "", err
	}
	response, err := h.recordMeasurement(caseID, 26.5, "Rui")
	if err != nil {
		return "", err
	}
	if err := expectStatus(response, http.StatusCreated); err != nil {
		return "", err
	}
	return caseID, nil
}

func (h *harness) readyCase(tag string) (string, error) {
	deviceID, err := h.createDevice(tag)
	if err != nil {
		return "", err
	}
	opened, err := h.openCase(deviceID)
	if err != nil {
		return "", err
	}
	caseID, err := nestedString(opened.Body, "case", "id")
	if err != nil {
		return "", err
	}
	response, err := h.recordMeasurement(caseID, 25.2, "Rui")
	if err != nil {
		return "", err
	}
	if err := expectStatus(response, http.StatusCreated); err != nil {
		return "", err
	}
	return caseID, nil
}

func expectStatus(response apiResponse, expected int) error {
	if response.Status != expected {
		return fmt.Errorf("status = %d, expected %d: %s", response.Status, expected, response.Raw)
	}
	return nil
}

func expectErrorCode(response apiResponse, expected string) error {
	code, err := nestedString(response.Body, "error", "code")
	if err != nil {
		return err
	}
	if code != expected {
		return fmt.Errorf("error code = %q, expected %q", code, expected)
	}
	return nil
}

func nestedString(root map[string]any, path ...string) (string, error) {
	value, err := nestedValue(root, path...)
	if err != nil {
		return "", err
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s is not a string", strings.Join(path, "."))
	}
	return text, nil
}

func nestedFloat(root map[string]any, path ...string) (float64, error) {
	value, err := nestedValue(root, path...)
	if err != nil {
		return 0, err
	}
	number, ok := value.(float64)
	if !ok {
		return 0, fmt.Errorf("%s is not a number", strings.Join(path, "."))
	}
	return number, nil
}

func nestedBool(root map[string]any, path ...any) (bool, error) {
	value, err := nestedAny(root, path...)
	if err != nil {
		return false, err
	}
	flag, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("%v is not a boolean", path)
	}
	return flag, nil
}

func nestedSlice(root map[string]any, key string) ([]any, error) {
	value, ok := root[key]
	if !ok {
		return nil, fmt.Errorf("missing %s", key)
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s is not an array", key)
	}
	return items, nil
}

func hasEvent(root map[string]any, kind string) bool {
	items, err := nestedSlice(root, "events")
	if err != nil {
		return false
	}
	for _, item := range items {
		event, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if event["kind"] == kind {
			return true
		}
	}
	return false
}

func nestedValue(root map[string]any, path ...string) (any, error) {
	parts := make([]any, 0, len(path))
	for _, part := range path {
		parts = append(parts, part)
	}
	return nestedAny(root, parts...)
}

func nestedAny(root map[string]any, path ...any) (any, error) {
	var current any = root
	for _, part := range path {
		switch typed := part.(type) {
		case string:
			object, ok := current.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s is not an object", typed)
			}
			value, ok := object[typed]
			if !ok {
				return nil, fmt.Errorf("missing %s", typed)
			}
			current = value
		case int:
			items, ok := current.([]any)
			if !ok {
				return nil, fmt.Errorf("index %d is not inside an array", typed)
			}
			if typed < 0 || typed >= len(items) {
				return nil, fmt.Errorf("index %d is out of range", typed)
			}
			current = items[typed]
		default:
			return nil, fmt.Errorf("unsupported path element %T", part)
		}
	}
	return current, nil
}
