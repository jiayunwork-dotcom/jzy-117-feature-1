package server

import (
	"fmt"
	"math"
	"net/http"
	"testing"
	"time"
)

// Case 1: the friend's pinned single-reach case through the full HTTP path,
// reference values nailed into the regression test (±1 cm).
func TestHTTP_PinnedBackwater(t *testing.T) {
	h, _ := newTestServer(t, 0)
	id := mustCreateFriendChannel(t, h, 1.0)

	job := submitAndWait(t, h, id, 1)
	if job["status"] != "succeeded" {
		t.Fatalf("job = %v", job)
	}
	res := getResult(t, h, id, 1)
	prof := res["profile"].(map[string]any)
	if got := prof["control_depth_m"].(float64); math.Abs(got-1.939) > 0.01 {
		t.Errorf("control depth = %.4f, want 1.939 ±0.01", got)
	}
	want := map[float64]float64{0: 1.939, 1000: 1.276, 2000: 1.090}
	for xi, wd := range want {
		if got := depthFromResult(t, res, xi); math.Abs(got-wd) > 0.01 {
			t.Errorf("xi=%v depth = %.4f, want %.4f ±0.01", xi, got, wd)
		}
	}
	reaches := prof["reaches"].([]any)
	r0 := reaches[0].(map[string]any)
	yn := r0["normal_depth_m"].(float64)
	for _, xi := range []float64{3500, 4000, 6000} {
		if got := depthFromResult(t, res, xi); math.Abs(got-yn) > 0.01 {
			t.Errorf("xi=%v depth %.4f not within 1 cm of yn %.4f", xi, got, yn)
		}
	}
	// Monotone non-increasing upstream and never below normal depth.
	points := r0["points"].([]any)
	prev := math.Inf(1)
	for _, pv := range points {
		y := pv.(map[string]any)["depth_m"].(float64)
		if y > prev+1e-9 {
			t.Fatalf("depth increases upstream: %.6f after %.6f", y, prev)
		}
		if y < yn-1e-9 {
			t.Fatalf("depth %.6f below normal depth %.6f", y, yn)
		}
		prev = y
	}
	end := prof["upstream_end"].(map[string]any)
	if end["at_normal_depth"] != true {
		t.Errorf("upstream end not at normal depth: %v", end)
	}
}

// Case 2: control depth = normal depth -> uniform profile.
func TestHTTP_ControlAtNormalDepth(t *testing.T) {
	h, _ := newTestServer(t, 0)
	id := mustCreateFriendChannel(t, h, 1.0)
	// First compute to learn yn from the result, then create a depth-control
	// version pinned to it.
	res := getResult(t, h, id, waitSucceeded(t, h, id))
	yn := res["profile"].(map[string]any)["reaches"].([]any)[0].(map[string]any)["normal_depth_m"].(float64)

	status, out := reqJSON(t, h, http.MethodPost,
		fmt.Sprintf("/v1/channels/%s/versions", id), map[string]any{
			"base_version": 1,
			"flow":         5.0,
			"reaches":      friendChannelBody(1.0)["reaches"],
			"control":      map[string]any{"type": "depth", "depth": yn},
		})
	if status != http.StatusCreated {
		t.Fatalf("add version: %d %v", status, out)
	}
	job := submitAndWait(t, h, id, 2)
	if job["status"] != "succeeded" {
		t.Fatalf("job = %v", job)
	}
	res2 := getResult(t, h, id, 2)
	points := res2["profile"].(map[string]any)["reaches"].([]any)[0].(map[string]any)["points"].([]any)
	for _, pv := range points {
		p := pv.(map[string]any)
		if math.Abs(p["depth_m"].(float64)-yn) > 2e-6 {
			t.Fatalf("xi=%v depth %.6f != yn %.6f", p["distance_from_control_m"], p["depth_m"], yn)
		}
	}
}

func waitSucceeded(t *testing.T, h http.Handler, id string) int {
	t.Helper()
	job := submitAndWait(t, h, id, 1)
	if job["status"] != "succeeded" {
		t.Fatalf("job = %v", job)
	}
	return 1
}

// Case 3: M2 drawdown with a depth control between yc and yn.
func TestHTTP_M2Drawdown(t *testing.T) {
	h, _ := newTestServer(t, 0)
	id := mustCreateFriendChannel(t, h, 1.0)
	res := getResult(t, h, id, waitSucceeded(t, h, id))
	prof0 := res["profile"].(map[string]any)
	r0 := prof0["reaches"].([]any)[0].(map[string]any)
	yn, yc := r0["normal_depth_m"].(float64), r0["critical_depth_m"].(float64)
	mid := (yn + yc) / 2

	reqJSON(t, h, http.MethodPost, fmt.Sprintf("/v1/channels/%s/versions", id), map[string]any{
		"base_version": 1, "flow": 5.0,
		"reaches": friendChannelBody(1.0)["reaches"],
		"control": map[string]any{"type": "depth", "depth": mid},
	})
	submitAndWait(t, h, id, 2)
	res2 := getResult(t, h, id, 2)
	r := res2["profile"].(map[string]any)["reaches"].([]any)[0].(map[string]any)
	if r["curve_type"] != "drawdown" {
		t.Errorf("curve_type = %v, want drawdown", r["curve_type"])
	}
	points := r["points"].([]any)
	prev := 0.0
	for i, pv := range points {
		y := pv.(map[string]any)["depth_m"].(float64)
		if i > 0 && y < prev-1e-9 {
			t.Fatal("depth decreases upstream in M2 push")
		}
		if y > yn+1e-6 {
			t.Fatalf("depth %.6f crosses above yn %.6f", y, yn)
		}
		prev = y
	}
}

// Case 4: raising only the crest height never lowers the water depth.
func TestHTTP_HigherWeirNeverLowersDepth(t *testing.T) {
	h, _ := newTestServer(t, 0)
	id := mustCreateFriendChannel(t, h, 1.0)
	submitAndWait(t, h, id, 1)
	low := getResult(t, h, id, 1)

	status, out := reqJSON(t, h, http.MethodPost,
		fmt.Sprintf("/v1/channels/%s/versions", id), newFriendWithCrest(1.5))
	if status != http.StatusCreated {
		t.Fatalf("add version: %d %v", status, out)
	}
	submitAndWait(t, h, id, 2)
	high := getResult(t, h, id, 2)

	lp := low["profile"].(map[string]any)["reaches"].([]any)[0].(map[string]any)["points"].([]any)
	for _, pv := range lp {
		p := pv.(map[string]any)
		xi := p["distance_from_control_m"].(float64)
		lo := p["depth_m"].(float64)
		hi := depthFromResult(t, high, xi)
		if hi+1e-9 < lo {
			t.Fatalf("xi=%v depth lowered %.4f -> %.4f after raising the weir", xi, lo, hi)
		}
	}
	cmp := high["comparison"].(map[string]any)
	if cmp["max_depth_decrease_m"].(float64) < -1e-9 {
		t.Errorf("unexpected depth decrease: %v", cmp["max_depth_decrease_m"])
	}
}

// helper kept tiny: build the v2 body map.
func newFriendWithCrest(crest float64) map[string]any {
	body := friendChannelBody(crest)
	body["base_version"] = 1
	return body
}

// Case 5: two reaches; only the upstream reach's roughness changes. The
// downstream reach must be unchanged; profile is recomputed wholesale.
func TestHTTP_UpstreamRoughnessLeavesDownstreamReach(t *testing.T) {
	h, _ := newTestServer(t, 0)
	body := twoReachBody(0.015)
	status, out := reqJSON(t, h, http.MethodPost, "/v1/channels", body)
	if status != http.StatusCreated {
		t.Fatalf("create: %d %v", status, out)
	}
	id := out["id"].(string)
	submitAndWait(t, h, id, 1)
	old := getResult(t, h, id, 1)

	v2 := twoReachBody(0.025)
	v2["base_version"] = 1
	if status, out := reqJSON(t, h, http.MethodPost,
		fmt.Sprintf("/v1/channels/%s/versions", id), v2); status != http.StatusCreated {
		t.Fatalf("add version: %d %v", status, out)
	}
	submitAndWait(t, h, id, 2)
	newRes := getResult(t, h, id, 2)

	// Downstream reach index 1, length 3000, xi in [0,3000].
	const tol = 1e-6
	oldR1 := old["profile"].(map[string]any)["reaches"].([]any)[1].(map[string]any)
	for xi := 0.0; xi <= 3000.0; xi += 20 {
		a, b := depthAtInReach(t, oldR1, xi), depthFromResult(t, newRes, xi)
		if math.Abs(a-b) > tol {
			t.Fatalf("downstream reach changed at xi=%v: %.7f -> %.7f", xi, a, b)
		}
	}
	cmp := newRes["comparison"].(map[string]any)
	if d := cmp["downstream_reach_max_delta_m"].(float64); d > tol {
		t.Errorf("comparison downstream delta = %.2e, want <= %.0e", d, tol)
	}
	// Upstream roughness went up: depths somewhere must increase.
	if cmp["max_abs_delta_m"].(float64) <= 0.001 {
		t.Errorf("expected a visible upstream change, comparison = %v", cmp)
	}
}

func depthAtInReach(t *testing.T, reach map[string]any, xi float64) float64 {
	t.Helper()
	for _, pv := range reach["points"].([]any) {
		p := pv.(map[string]any)
		if numEq(p["distance_from_control_m"].(float64), xi, 1e-6) {
			return p["depth_m"].(float64)
		}
	}
	t.Fatalf("no station at xi=%v", xi)
	return 0
}

func twoReachBody(upstreamN float64) map[string]any {
	return map[string]any{
		"name": "two", "flow": 5.0,
		"reaches": []map[string]any{
			{
				"name": "up", "length": 3000.0,
				"section":   map[string]any{"bottom_width": 3.0, "side_slope": 0.0},
				"roughness": upstreamN, "slope": 0.001,
			},
			{
				"name": "down", "length": 3000.0,
				"section":   map[string]any{"bottom_width": 3.0, "side_slope": 0.0},
				"roughness": 0.015, "slope": 0.001,
			},
		},
		"control": map[string]any{
			"type": "weir",
			"weir": map[string]any{"width": 3.0, "crest_height": 1.0, "discharge_coefficient": 0.62},
		},
	}
}

// Case 6: steep reach and critical-crossing jobs fail with a location.
func TestHTTP_HydraulicFailures(t *testing.T) {
	h, _ := newTestServer(t, 0)

	t.Run("steep reach", func(t *testing.T) {
		body := twoReachBody(0.015)
		up := body["reaches"].([]map[string]any)[0]
		up["slope"] = 0.02
		up["roughness"] = 0.012
		status, out := reqJSON(t, h, http.MethodPost, "/v1/channels", body)
		if status != http.StatusCreated {
			t.Fatalf("create: %d %v", status, out)
		}
		id := out["id"].(string)
		job := submitAndWait(t, h, id, 1)
		if job["status"] != "failed" {
			t.Fatalf("status = %v, want failed", job["status"])
		}
		fail := job["failure"].(map[string]any)
		if fail["code"] != "steep_reach" {
			t.Errorf("code = %v", fail["code"])
		}
		if int(fail["reach_index"].(float64)) != 0 {
			t.Errorf("reach_index = %v, want 0", fail["reach_index"])
		}
		if _, ok := fail["distance_from_control_m"]; !ok {
			t.Error("failure lacks distance_from_control_m")
		}
	})

	t.Run("control below critical", func(t *testing.T) {
		body := friendChannelBody(1.0)
		body["control"] = map[string]any{"type": "depth", "depth": 0.5}
		id := mustCreateFromBody(t, h, body)
		job := submitAndWait(t, h, id, 1)
		if job["status"] != "failed" {
			t.Fatalf("status = %v", job["status"])
		}
		fail := job["failure"].(map[string]any)
		if fail["code"] != "control_depth_not_subcritical" {
			t.Errorf("code = %v", fail["code"])
		}
	})
}

func mustCreateFromBody(t *testing.T, h http.Handler, body map[string]any) string {
	t.Helper()
	status, out := reqJSON(t, h, http.MethodPost, "/v1/channels", body)
	if status != http.StatusCreated {
		t.Fatalf("create: %d %v", status, out)
	}
	return out["id"].(string)
}

// Case 7a: optimistic concurrency — a stale base version is rejected.
func TestHTTP_VersionConflict(t *testing.T) {
	h, _ := newTestServer(t, 0)
	id := mustCreateFriendChannel(t, h, 1.0)

	// v2 from base 1 succeeds.
	body := friendChannelBody(1.2)
	body["base_version"] = 1
	if status, out := reqJSON(t, h, http.MethodPost,
		fmt.Sprintf("/v1/channels/%s/versions", id), body); status != http.StatusCreated {
		t.Fatalf("v2: %d %v", status, out)
	}
	// Another edit still based on v1 must be rejected.
	body = friendChannelBody(1.3)
	body["base_version"] = 1
	status, out := reqJSON(t, h, http.MethodPost,
		fmt.Sprintf("/v1/channels/%s/versions", id), body)
	if status != http.StatusConflict {
		t.Fatalf("stale submit: status = %d, want 409, body %v", status, out)
	}
	if out["code"] != "version_conflict" {
		t.Errorf("code = %v", out["code"])
	}
}

// Case 7b + 7c: persistence across restart, and stale results flagged.
func TestHTTP_RestartPersistence(t *testing.T) {
	h, dep := newTestServer(t, 0)
	id := mustCreateFriendChannel(t, h, 1.0)
	submitAndWait(t, h, id, 1)
	oldRes := getResult(t, h, id, 1)
	controlDepth := oldRes["profile"].(map[string]any)["control_depth_m"].(float64)

	// Restart with the same data directory: channel, history and result load.
	dep.runner.Kill()
	dep2, h2 := reopen(t, dep.store.Dir(), 0)
	defer dep2.runner.Shutdown()
	status, ch := reqJSON(t, h2, http.MethodGet, "/v1/channels/"+id, nil)
	if status != http.StatusOK || ch["current_version"].(float64) != 1 {
		t.Fatalf("channel after restart: %d %v", status, ch)
	}
	res := getResult(t, h2, id, 1)
	got := res["profile"].(map[string]any)["control_depth_m"].(float64)
	if math.Abs(got-controlDepth) > 1e-9 {
		t.Errorf("result changed across restart: %.9f vs %.9f", got, controlDepth)
	}
	if res["stale"] != false {
		t.Errorf("result should not be stale: %v", res["stale"])
	}

	// Add v2 after restart; v1's result is now stale.
	body := friendChannelBody(1.3)
	body["base_version"] = 1
	if status, out := reqJSON(t, h2, http.MethodPost,
		fmt.Sprintf("/v1/channels/%s/versions", id), body); status != http.StatusCreated {
		t.Fatalf("v2 after restart: %d %v", status, out)
	}
	staleRes := getResult(t, h2, id, 1)
	if staleRes["stale"] != true {
		t.Errorf("v1 result should be flagged stale, got %v", staleRes["stale"])
	}
}

// Case 7c: a job unfinished at restart becomes interrupted, and the same
// version can be submitted again.
func TestHTTP_InterruptedAfterRestart(t *testing.T) {
	// Delay makes the job long enough to be mid-flight when we "restart".
	h, dep := newTestServer(t, 5*time.Millisecond)
	id := mustCreateFriendChannel(t, h, 1.0)
	status, out := reqJSON(t, h, http.MethodPost,
		fmt.Sprintf("/v1/channels/%s/versions/1/profile-jobs", id), nil)
	if status != http.StatusAccepted {
		t.Fatalf("submit: %d %v", status, out)
	}
	jobID := out["id"].(string)

	// Wait until it really runs with progress > 0.
	deadline := time.Now().Add(5 * time.Second)
	var progressed bool
	for time.Now().Before(deadline) {
		_, j := reqJSON(t, h, http.MethodGet, "/v1/jobs/"+jobID, nil)
		if j["status"] == "running" && j["progress"].(float64) > 0 {
			progressed = true
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !progressed {
		t.Fatal("never observed a running job with advancing progress")
	}

	// Hard stop (as in a killed container), then restart with no delay.
	dep.runner.Kill()
	dep2, h2 := reopen(t, dep.store.Dir(), 0)
	defer dep2.runner.Shutdown()
	_, j := reqJSON(t, h2, http.MethodGet, "/v1/jobs/"+jobID, nil)
	if j["status"] != "interrupted" {
		t.Fatalf("status after restart = %v, want interrupted", j["status"])
	}

	// Resubmit same version -> a fresh job, succeeds.
	status, out2 := reqJSON(t, h2, http.MethodPost,
		fmt.Sprintf("/v1/channels/%s/versions/1/profile-jobs", id), nil)
	if status != http.StatusAccepted {
		t.Fatalf("resubmit after interrupt: %d %v", status, out2)
	}
	if out2["id"] == jobID {
		t.Error("resubmission should create a new job id")
	}
	fin := waitJob(t, h2, out2["id"].(string))
	if fin["status"] != "succeeded" {
		t.Fatalf("resubmitted job = %v", fin)
	}
}

// Case 8: a running job is cancelable, progress was observable, and no
// result is ever produced.
func TestHTTP_CancelRunningJob(t *testing.T) {
	h, _ := newTestServer(t, 5*time.Millisecond)
	id := mustCreateFriendChannel(t, h, 1.0)
	_, out := reqJSON(t, h, http.MethodPost,
		fmt.Sprintf("/v1/channels/%s/versions/1/profile-jobs", id), nil)
	jobID := out["id"].(string)

	// Observe advancing progress.
	first := -1.0
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, j := reqJSON(t, h, http.MethodGet, "/v1/jobs/"+jobID, nil)
		if p, ok := j["progress"].(float64); ok && p > 0 {
			first = p
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if first < 0 {
		t.Fatal("no progress observed")
	}

	status, j := reqJSON(t, h, http.MethodPost, "/v1/jobs/"+jobID+"/cancel", nil)
	if status != http.StatusOK {
		t.Fatalf("cancel: %d %v", status, j)
	}
	fin := waitJob(t, h, jobID)
	if fin["status"] != "canceled" {
		t.Fatalf("final status = %v, want canceled", fin["status"])
	}
	// No result exists.
	status, _ = reqJSON(t, h, http.MethodGet,
		fmt.Sprintf("/v1/channels/%s/versions/1/profile-result", id), nil)
	if status != http.StatusNotFound {
		t.Errorf("result after cancel: status = %d, want 404", status)
	}

	// Canceling again is a 409.
	status, _ = reqJSON(t, h, http.MethodPost, "/v1/jobs/"+jobID+"/cancel", nil)
	if status != http.StatusConflict {
		t.Errorf("second cancel = %d, want 409", status)
	}
}

// Duplicate submission while a job is active is idempotent.
func TestHTTP_DuplicateSubmissionReuses(t *testing.T) {
	h, _ := newTestServer(t, 5*time.Millisecond)
	id := mustCreateFriendChannel(t, h, 1.0)
	_, first := reqJSON(t, h, http.MethodPost,
		fmt.Sprintf("/v1/channels/%s/versions/1/profile-jobs", id), nil)
	_, second := reqJSON(t, h, http.MethodPost,
		fmt.Sprintf("/v1/channels/%s/versions/1/profile-jobs", id), nil)
	if second["id"] != first["id"] || second["reused"] != true {
		t.Fatalf("duplicate submit not reused: %v vs %v", first, second)
	}
	fin := waitJob(t, h, first["id"].(string))
	if fin["status"] != "succeeded" {
		t.Fatalf("job = %v", fin)
	}
	// After success, submission reuses the result.
	_, third := reqJSON(t, h, http.MethodPost,
		fmt.Sprintf("/v1/channels/%s/versions/1/profile-jobs", id), nil)
	if third["id"] != first["id"] || third["reused"] != true {
		t.Fatalf("post-success submit not reused: %v", third)
	}
}
