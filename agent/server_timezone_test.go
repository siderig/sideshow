package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The timezone handlers only touch s.tz, so a minimal Server exercises them
// end-to-end without standing up the whole agent — and without any external
// network (the detect endpoint's live path is never invoked here).
func newTZServer() *Server { return &Server{tz: NewTimezone()} }

func TestHandleTimezoneGET(t *testing.T) {
	s := newTZServer()
	rr := httptest.NewRecorder()
	s.handleTimezone(rr, httptest.NewRequest(http.MethodGet, "/api/timezone", nil))
	if rr.Code != 200 {
		t.Fatalf("GET status = %d, want 200", rr.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET body not JSON: %v", err)
	}
	if _, ok := body["current"]; !ok {
		t.Errorf("GET body missing 'current': %v", body)
	}
	if _, ok := body["can_set"]; !ok {
		t.Errorf("GET body missing 'can_set': %v", body)
	}
	if _, ok := body["zones"]; !ok {
		t.Errorf("GET body missing 'zones': %v", body)
	}
}

func TestHandleTimezoneSetInvalid(t *testing.T) {
	s := newTZServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/timezone", strings.NewReader(`{"zone":"bad name"}`))
	s.handleTimezone(rr, req)
	if rr.Code != 400 {
		t.Errorf("POST invalid zone status = %d, want 400", rr.Code)
	}
}

func TestHandleTimezoneMethod(t *testing.T) {
	s := newTZServer()
	rr := httptest.NewRecorder()
	s.handleTimezone(rr, httptest.NewRequest(http.MethodPut, "/api/timezone", nil))
	if rr.Code != 405 {
		t.Errorf("PUT status = %d, want 405", rr.Code)
	}
}

func TestHandleTimezoneDetectMethod(t *testing.T) {
	s := newTZServer()
	rr := httptest.NewRecorder()
	// GET (wrong method) must be rejected before any outbound request is made.
	s.handleTimezoneDetect(rr, httptest.NewRequest(http.MethodGet, "/api/timezone/detect", nil))
	if rr.Code != 405 {
		t.Errorf("GET detect status = %d, want 405", rr.Code)
	}
}
