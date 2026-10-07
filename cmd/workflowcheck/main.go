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
		"adjust-case-priority":     checkAdjustCasePriority,
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
	position, err := nestedFloat(opened.Body, "queue_position")
	if err != nil {
		return err
	}
	if position != 1 {
		return fmt.Errorf("sole open case queue position = %v, expected 1", position)
	}

	read, err := h.request(http.MethodGet, "/cases/"+caseID, nil)
	if err != nil {
		return err
	}
	if err := expectStatus(read, http.StatusOK); err != nil {
		return err
	}

	second, err := h.request(http.MethodPost, "/cases", map[string]any{
		"device_id":           deviceID,
		"nominal":             25.0,
		"tolerance":           0.5,
		"steward":             "Qiao",
		"priority":            3,
		"priority_reason":     "routine verification",
		"planned_complete_at": futureTime(48 * time.Hour),
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
	before, err := h.request(http.MethodGet, "/cases/"+caseID, nil)
	if err != nil {
		return err
	}
	if err := expectStatus(before, http.StatusOK); err != nil {
		return err
	}
	originalSequence, err := nestedFloat(before.Body, "case", "enqueue_sequence")
	if err != nil {
		return err
	}
	queuedPosition, err := nestedAny(before.Body, "queue_position")
	if err != nil {
		return err
	}
	if queuedPosition != nil {
		return fmt.Errorf("recheck case still occupies the queue: %v", queuedPosition)
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
	returnedSequence, err := nestedFloat(reopened.Body, "case", "enqueue_sequence")
	if err != nil {
		return err
	}
	if returnedSequence != originalSequence {
		return fmt.Errorf("reopen changed enqueue sequence: was %v, now %v", originalSequence, returnedSequence)
	}
	returnedPosition, err := nestedFloat(reopened.Body, "queue_position")
	if err != nil {
		return err
	}
	if returnedPosition != 1 {
		return fmt.Errorf("reopened case queue position = %v, expected 1", returnedPosition)
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

	// Two devices, two cases. The overdue case has a lower urgency rank,
	// proving overdue status is surfaced without changing queue order.
	overdueDevice, err := h.createDevice("MET-1501")
	if err != nil {
		return err
	}
	overdueCase, err := h.openCasePlanned(overdueDevice, "BATCH-Q1", 3, "routine calibration", futureTime(-2*time.Hour))
	if err != nil {
		return err
	}
	overdueID, err := nestedString(overdueCase.Body, "case", "id")
	if err != nil {
		return err
	}
	overdueFlag, err := nestedBool(overdueCase.Body, "overdue")
	if err != nil {
		return err
	}
	if !overdueFlag {
		return errors.New("past planned completion case was not reported overdue")
	}

	urgentDevice, err := h.createDevice("MET-1502")
	if err != nil {
		return err
	}
	urgentCase, err := h.openCasePlanned(urgentDevice, "BATCH-Q1", 1, "customer escalated", futureTime(48*time.Hour))
	if err != nil {
		return err
	}
	urgentID, err := nestedString(urgentCase.Body, "case", "id")
	if err != nil {
		return err
	}
	urgentPosition, err := nestedFloat(urgentCase.Body, "queue_position")
	if err != nil {
		return err
	}
	if urgentPosition != 1 {
		return fmt.Errorf("urgent case queue position = %v, expected 1", urgentPosition)
	}
	overdueRead, err := h.request(http.MethodGet, "/cases/"+overdueID, nil)
	if err != nil {
		return err
	}
	overduePosition, err := nestedFloat(overdueRead.Body, "queue_position")
	if err != nil {
		return err
	}
	if overduePosition != 2 {
		return fmt.Errorf("overdue case queue position = %v, expected 2", overduePosition)
	}
	overdueStillFlagged, err := nestedBool(overdueRead.Body, "overdue")
	if err != nil {
		return err
	}
	if !overdueStillFlagged {
		return errors.New("case read does not flag the overdue case")
	}

	queue, err := h.request(http.MethodGet, "/cases", nil)
	if err != nil {
		return err
	}
	if err := expectStatus(queue, http.StatusOK); err != nil {
		return err
	}
	headID, err := nestedString(queue.Body, "queue", 0, "case_id")
	if err != nil {
		return err
	}
	if headID != urgentID {
		return fmt.Errorf("queue head = %q, expected %q", headID, urgentID)
	}
	secondOverdue, err := nestedBool(queue.Body, "queue", 1, "overdue")
	if err != nil {
		return err
	}
	if !secondOverdue {
		return errors.New("queue listing does not flag the overdue case")
	}

	// A batch filter keeps global positions so the filtered head cannot
	// disagree with the enforcement rule.
	batch, err := h.request(http.MethodGet, "/cases?batch_id=BATCH-Q1", nil)
	if err != nil {
		return err
	}
	if err := expectStatus(batch, http.StatusOK); err != nil {
		return err
	}
	batchEntries, err := nestedSlice(batch.Body, "queue")
	if err != nil {
		return err
	}
	if len(batchEntries) != 2 {
		return fmt.Errorf("batch filter returned %d entries", len(batchEntries))
	}
	secondPosition, err := nestedFloat(batch.Body, "queue", 1, "position")
	if err != nil {
		return err
	}
	if secondPosition != 2 {
		return fmt.Errorf("filtered view rewrote global position: %v", secondPosition)
	}
	otherBatch, err := h.request(http.MethodGet, "/cases?batch_id=BATCH-OTHER", nil)
	if err != nil {
		return err
	}
	if err := expectStatus(otherBatch, http.StatusOK); err != nil {
		return err
	}
	empty, err := nestedSlice(otherBatch.Body, "queue")
	if err != nil {
		return err
	}
	if len(empty) != 0 {
		return fmt.Errorf("unknown batch returned %d entries", len(empty))
	}

	// The second case in line cannot enter measurement; only the head can.
	blocked, err := h.recordMeasurement(overdueID, 25.1, "Rui")
	if err != nil {
		return err
	}
	if err := expectStatus(blocked, http.StatusConflict); err != nil {
		return err
	}
	if err := expectErrorCode(blocked, "not_queue_head"); err != nil {
		return err
	}

	measured, err := h.recordMeasurement(urgentID, 25.1, "Rui")
	if err != nil {
		return err
	}
	if err := expectStatus(measured, http.StatusCreated); err != nil {
		return err
	}
	measuredPosition, err := nestedAny(measured.Body, "queue_position")
	if err != nil {
		return err
	}
	if measuredPosition != nil {
		return fmt.Errorf("measured case still holds a queue position: %v", measuredPosition)
	}

	// Once the head leaves the bench the overdue case becomes head and is
	// allowed into measurement: the displayed head and the enforced head
	// are the same case.
	queue, err = h.request(http.MethodGet, "/cases", nil)
	if err != nil {
		return err
	}
	newHeadID, err := nestedString(queue.Body, "queue", 0, "case_id")
	if err != nil {
		return err
	}
	if newHeadID != overdueID {
		return fmt.Errorf("new queue head = %q, expected %q", newHeadID, overdueID)
	}
	nowMeasured, err := h.recordMeasurement(overdueID, 25.1, "Rui")
	if err != nil {
		return err
	}
	return expectStatus(nowMeasured, http.StatusCreated)
}

func checkAdjustCasePriority() error {
	h := newHarness()
	defer h.close()

	normalDevice, err := h.createDevice("MET-1601")
	if err != nil {
		return err
	}
	normalCase, err := h.openCasePlanned(normalDevice, "BATCH-P1", 4, "normal workload", futureTime(72*time.Hour))
	if err != nil {
		return err
	}
	normalID, err := nestedString(normalCase.Body, "case", "id")
	if err != nil {
		return err
	}

	urgentDevice, err := h.createDevice("MET-1602")
	if err != nil {
		return err
	}
	urgentCase, err := h.openCasePlanned(urgentDevice, "BATCH-P1", 2, "customer escalated", futureTime(24*time.Hour))
	if err != nil {
		return err
	}
	urgentID, err := nestedString(urgentCase.Body, "case", "id")
	if err != nil {
		return err
	}

	queue, err := h.request(http.MethodGet, "/cases", nil)
	if err != nil {
		return err
	}
	headID, err := nestedString(queue.Body, "queue", 0, "case_id")
	if err != nil {
		return err
	}
	if headID != urgentID {
		return fmt.Errorf("initial queue head = %q", headID)
	}

	// Promoting the normal case above the urgent one reorders the queue and
	// records the new basis without touching the enqueue sequence.
	beforeSequence, err := nestedFloat(normalCase.Body, "case", "enqueue_sequence")
	if err != nil {
		return err
	}
	adjusted, err := h.request(http.MethodPost, "/cases/"+normalID+"/priority", map[string]any{
		"priority":        1,
		"priority_reason": "expedite: deadline moved up by the customer",
		"actor":           "Lin",
	})
	if err != nil {
		return err
	}
	if err := expectStatus(adjusted, http.StatusOK); err != nil {
		return err
	}
	afterPriority, err := nestedFloat(adjusted.Body, "case", "priority")
	if err != nil {
		return err
	}
	if afterPriority != 1 {
		return fmt.Errorf("adjusted priority = %v", afterPriority)
	}
	afterSequence, err := nestedFloat(adjusted.Body, "case", "enqueue_sequence")
	if err != nil {
		return err
	}
	if afterSequence != beforeSequence {
		return fmt.Errorf("priority change rewrote enqueue sequence: %v -> %v", beforeSequence, afterSequence)
	}
	afterPosition, err := nestedFloat(adjusted.Body, "queue_position")
	if err != nil {
		return err
	}
	if afterPosition != 1 {
		return fmt.Errorf("promoted case queue position = %v", afterPosition)
	}
	events, err := h.request(http.MethodGet, "/cases/"+normalID+"/events", nil)
	if err != nil {
		return err
	}
	if !hasEvent(events.Body, "priority_adjusted") {
		return errors.New("priority_adjusted audit event is missing")
	}

	queue, err = h.request(http.MethodGet, "/cases", nil)
	if err != nil {
		return err
	}
	newHeadID, err := nestedString(queue.Body, "queue", 0, "case_id")
	if err != nil {
		return err
	}
	if newHeadID != normalID {
		return fmt.Errorf("queue head after promotion = %q, expected %q", newHeadID, normalID)
	}

	// Invalid input and sealed history are rejected.
	badRank, err := h.request(http.MethodPost, "/cases/"+normalID+"/priority", map[string]any{
		"priority":        9,
		"priority_reason": "bad rank",
		"actor":           "Lin",
	})
	if err != nil {
		return err
	}
	if err := expectStatus(badRank, http.StatusBadRequest); err != nil {
		return err
	}
	if err := expectErrorCode(badRank, "invalid_input"); err != nil {
		return err
	}
	missingBasis, err := h.request(http.MethodPost, "/cases/"+normalID+"/priority", map[string]any{
		"priority": 2,
		"actor":    "Lin",
	})
	if err != nil {
		return err
	}
	if err := expectStatus(missingBasis, http.StatusBadRequest); err != nil {
		return err
	}

	// Finish and seal the urgent case. The promoted normal case is now head
	// of the queue, so the bench processes it first; the queue head gate
	// applies here exactly as it does in production.
	normalMeasured, err := h.recordMeasurement(normalID, 25.1, "Rui")
	if err != nil {
		return err
	}
	if err := expectStatus(normalMeasured, http.StatusCreated); err != nil {
		return err
	}
	urgentMeasured, err := h.recordMeasurement(urgentID, 25.1, "Rui")
	if err != nil {
		return err
	}
	if err := expectStatus(urgentMeasured, http.StatusCreated); err != nil {
		return err
	}
	sealed, err := h.request(http.MethodPost, "/cases/"+urgentID+"/seal", map[string]any{
		"reviewer": "Mira",
		"summary":  "accepted measurement sealed for review",
	})
	if err != nil {
		return err
	}
	if err := expectStatus(sealed, http.StatusCreated); err != nil {
		return err
	}
	reprioritizeSealed, err := h.request(http.MethodPost, "/cases/"+urgentID+"/priority", map[string]any{
		"priority":        5,
		"priority_reason": "attempt to rewrite finished history",
		"actor":           "Lin",
	})
	if err != nil {
		return err
	}
	if err := expectStatus(reprioritizeSealed, http.StatusConflict); err != nil {
		return err
	}
	read, err := h.request(http.MethodGet, "/cases/"+urgentID, nil)
	if err != nil {
		return err
	}
	storedPriority, err := nestedFloat(read.Body, "case", "priority")
	if err != nil {
		return err
	}
	if storedPriority != 2 {
		return fmt.Errorf("sealed case priority changed to %v", storedPriority)
	}
	history, err := nestedSlice(read.Body, "measurements")
	if err != nil {
		return err
	}
	if len(history) != 1 {
		return fmt.Errorf("sealed measurement history changed: %d entries", len(history))
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
	return h.openCasePlanned(deviceID, "", 3, "routine verification", futureTime(48*time.Hour))
}

func (h *harness) openCasePlanned(deviceID, batchID string, priority int, reason string, plannedCompleteAt string) (apiResponse, error) {
	response, err := h.request(http.MethodPost, "/cases", map[string]any{
		"device_id":           deviceID,
		"batch_id":            batchID,
		"nominal":             25.0,
		"tolerance":           0.5,
		"steward":             "Qiao",
		"priority":            priority,
		"priority_reason":     reason,
		"planned_complete_at": plannedCompleteAt,
	})
	if err != nil {
		return apiResponse{}, err
	}
	if err := expectStatus(response, http.StatusCreated); err != nil {
		return apiResponse{}, err
	}
	return response, nil
}

func futureTime(offset time.Duration) string {
	return time.Now().Add(offset).UTC().Format(time.RFC3339)
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

func nestedString(root map[string]any, path ...any) (string, error) {
	value, err := nestedAny(root, path...)
	if err != nil {
		return "", err
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%v is not a string", path)
	}
	return text, nil
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

func nestedFloat(root map[string]any, path ...any) (float64, error) {
	value, err := nestedAny(root, path...)
	if err != nil {
		return 0, err
	}
	number, ok := value.(float64)
	if !ok {
		return 0, fmt.Errorf("%v is not a number", path)
	}
	return number, nil
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
