package server

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

func do(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var out map[string]any
	// Framework-generated replies (e.g. 405 Method Not Allowed) are plain
	// text; only parse bodies our handlers marked as JSON.
	if rec.Body.Len() > 0 && rec.Header().Get("Content-Type") == "application/json" {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("response is not JSON: %v (%s)", err, rec.Body.String())
		}
	}
	return rec.Code, out
}

func TestUniformFlowEndpoint(t *testing.T) {
	h := New()
	status, out := do(t, h, http.MethodPost, "/v1/uniform-flow",
		`{"bottom_width":2,"side_slope":0,"roughness":0.02,"slope":0.001,"flow":3}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, out)
	}

	yn := out["normal_depth"].(float64)
	if math.Abs(yn-1.3677515051663782) > 1e-9 {
		t.Errorf("normal_depth = %.12f, want 1.3677515052", yn)
	}
	if math.Abs(out["velocity"].(float64)-1.0966904399915354) > 1e-9 {
		t.Errorf("velocity = %v", out["velocity"])
	}
	if math.Abs(out["froude_number"].(float64)-0.29939597210037816) > 1e-9 {
		t.Errorf("froude_number = %v", out["froude_number"])
	}
	r := out["hydraulic_radius"].(float64)
	if math.Abs(r-0.5776583827238528) > 1e-9 {
		t.Errorf("hydraulic_radius = %v", r)
	}
	if out["regime"] != "subcritical" {
		t.Errorf("regime = %v", out["regime"])
	}
	// Rectangular section: surface width must stay the bottom width.
	if out["top_width"].(float64) != 2.0 {
		t.Errorf("top_width = %v, want 2", out["top_width"])
	}
}

func TestUniformFlowTrapezoid(t *testing.T) {
	h := New()
	status, out := do(t, h, http.MethodPost, "/v1/uniform-flow",
		`{"bottom_width":3,"side_slope":1.5,"roughness":0.025,"slope":0.0008,"flow":10}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, out)
	}
	yn := out["normal_depth"].(float64)
	if !(yn > 0 && math.IsNaN(yn) == false) {
		t.Errorf("bad normal depth %v", yn)
	}
	if fr := out["froude_number"].(float64); fr <= 0 || fr > 1 {
		t.Errorf("expected a positive subcritical Froude number, got %v", fr)
	}
}

func TestUniformFlowValidation(t *testing.T) {
	h := New()
	cases := []struct {
		name string
		body string
		fld  string
	}{
		{"bad roughness", `{"bottom_width":2,"roughness":0,"slope":0.001,"flow":3}`, "roughness"},
		{"bad slope", `{"bottom_width":2,"roughness":0.02,"slope":-0.001,"flow":3}`, "slope"},
		{"bad width", `{"bottom_width":-2,"roughness":0.02,"slope":0.001,"flow":3}`, "bottom_width"},
		{"negative flow", `{"bottom_width":2,"roughness":0.02,"slope":0.001,"flow":-3}`, "flow"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, out := do(t, h, http.MethodPost, "/v1/uniform-flow", tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %v", status, out)
			}
			if out["field"] != tc.fld {
				t.Errorf("field = %v, want %q", out["field"], tc.fld)
			}
		})
	}
}

func TestWeirEndpoint(t *testing.T) {
	h := New()
	status, out := do(t, h, http.MethodPost, "/v1/weir-flow",
		`{"width":0.8,"head":0.25}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, out)
	}
	if math.Abs(out["flow"].(float64)-0.1830838059468942) > 1e-9 {
		t.Errorf("flow = %v", out["flow"])
	}
	if out["discharge_coefficient"].(float64) != 0.62 {
		t.Errorf("default Cd = %v, want 0.62", out["discharge_coefficient"])
	}
	if out["note"] == nil || out["note"] == "" {
		t.Error("expected a datum note; head must not silently replace normal depth")
	}
}

func TestWeirCustomCoefficientAndValidation(t *testing.T) {
	h := New()
	status, out := do(t, h, http.MethodPost, "/v1/weir-flow",
		`{"width":1,"head":0.2,"discharge_coefficient":0.6}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, out)
	}
	if out["discharge_coefficient"].(float64) != 0.6 {
		t.Errorf("Cd echoed = %v, want 0.6", out["discharge_coefficient"])
	}

	for _, body := range []string{
		`{"width":0,"head":0.2}`,
		`{"width":1,"head":-0.2}`,
		`{"width":1,"head":0.2,"discharge_coefficient":0}`,
	} {
		if code, _ := do(t, h, http.MethodPost, "/v1/weir-flow", body); code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, code)
		}
	}
}

func TestMalformedAndUnknownFields(t *testing.T) {
	h := New()
	if status, _ := do(t, h, http.MethodPost, "/v1/weir-flow", `{not json`); status != http.StatusBadRequest {
		t.Errorf("malformed JSON status = %d, want 400", status)
	}
	if status, _ := do(t, h, http.MethodPost, "/v1/weir-flow",
		`{"width":1,"head":0.2,"bogus":1}`); status != http.StatusBadRequest {
		t.Errorf("unknown field status = %d, want 400", status)
	}
}

func TestHealthAndMethodRouting(t *testing.T) {
	h := New()
	if status, out := do(t, h, http.MethodGet, "/healthz", ""); status != http.StatusOK || out["status"] != "ok" {
		t.Errorf("health = %d %v", status, out)
	}
	if status, _ := do(t, h, http.MethodGet, "/v1/weir-flow", "{}"); status != http.StatusMethodNotAllowed {
		t.Errorf("GET on POST endpoint = %d, want 405", status)
	}
	if status, _ := do(t, h, http.MethodPost, "/v1/nope", "{}"); status != http.StatusNotFound {
		t.Errorf("unknown path = %d, want 404", status)
	}
}
