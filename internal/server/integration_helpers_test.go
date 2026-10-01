package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"openchannel/internal/runner"
	"openchannel/internal/store"
)

// newTestServer builds a handler backed by a fresh temp data directory.
// stepDelay slows integration steps enough that a running job can be
// observed and canceled deterministically.
func newTestServer(t *testing.T, stepDelay time.Duration) (http.Handler, *dependencies) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rn := runner.New(st, stepDelay)
	t.Cleanup(rn.Shutdown)
	dep := &dependencies{store: st, runner: rn}
	return newMux(dep), dep
}

func reqJSON(t *testing.T, h http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 && rec.Header().Get("Content-Type") == "application/json" {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("non-JSON body %q: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, out
}

func mustCreateFriendChannel(t *testing.T, h http.Handler, crestHeight float64) string {
	t.Helper()
	body := friendChannelBody(crestHeight)
	status, out := reqJSON(t, h, http.MethodPost, "/v1/channels", body)
	if status != http.StatusCreated {
		t.Fatalf("create channel: %d %v", status, out)
	}
	return out["id"].(string)
}

func friendChannelBody(crestHeight float64) map[string]any {
	return map[string]any{
		"name": "friend",
		"flow": 5.0,
		"reaches": []map[string]any{{
			"name":      "main",
			"length":    6000.0,
			"section":   map[string]any{"bottom_width": 3.0, "side_slope": 0.0},
			"roughness": 0.015,
			"slope":     0.001,
		}},
		"control": map[string]any{
			"type": "weir",
			"weir": map[string]any{
				"width":                 3.0,
				"crest_height":          crestHeight,
				"discharge_coefficient": 0.62,
			},
		},
	}
}

func submitAndWait(t *testing.T, h http.Handler, channelID string, version int) map[string]any {
	t.Helper()
	status, out := reqJSON(t, h, http.MethodPost,
		fmt.Sprintf("/v1/channels/%s/versions/%d/profile-jobs", channelID, version), nil)
	if status != http.StatusAccepted && status != http.StatusOK {
		t.Fatalf("submit: %d %v", status, out)
	}
	jobID := out["id"].(string)
	return waitJob(t, h, jobID)
}

func waitJob(t *testing.T, h http.Handler, jobID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		status, out := reqJSON(t, h, http.MethodGet, "/v1/jobs/"+jobID, nil)
		if status != http.StatusOK {
			t.Fatalf("get job: %d %v", status, out)
		}
		last = out
		switch out["status"] {
		case "succeeded", "failed", "canceled", "interrupted":
			return out
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish: %v", jobID, last)
	return nil
}

func getResult(t *testing.T, h http.Handler, channelID string, version int) map[string]any {
	t.Helper()
	status, out := reqJSON(t, h, http.MethodGet,
		fmt.Sprintf("/v1/channels/%s/versions/%d/profile-result", channelID, version), nil)
	if status != http.StatusOK {
		t.Fatalf("get result: %d %v", status, out)
	}
	return out
}

// depthFromResult interpolates? No — stations are exactly on the 20 m grid,
// so xi values used in tests (multiples of 20) are present directly.
func depthFromResult(t *testing.T, result map[string]any, xi float64) float64 {
	t.Helper()
	prof := result["profile"].(map[string]any)
	reaches := prof["reaches"].([]any)
	for _, rv := range reaches {
		reach := rv.(map[string]any)
		points := reach["points"].([]any)
		for _, pv := range points {
			p := pv.(map[string]any)
			if numEq(p["distance_from_control_m"].(float64), xi, 1e-6) {
				return p["depth_m"].(float64)
			}
		}
	}
	t.Fatalf("no station at xi=%v", xi)
	return 0
}

func numEq(a, b, tol float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= tol
}

// reopen starts a second store+runner over an existing data directory,
// exactly as a restarted container would.
func reopen(t *testing.T, dir string, stepDelay time.Duration) (*dependencies, http.Handler) {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rn := runner.New(st, stepDelay)
	t.Cleanup(rn.Shutdown)
	dep := &dependencies{store: st, runner: rn}
	return dep, newMux(dep)
}
