package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"openchannel/internal/jobs"
	"openchannel/internal/store"
)

// --- integration harness with a real on-disk store ---

type harness struct {
	t     *testing.T
	dir   string
	st    *store.Store
	mgr   *jobs.Manager
	h     http.Handler
	mu    sync.Mutex
	steps []float64
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	st, err := store.Open(filepath.Join(dir, "openchannel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	hv := &harness{t: t, dir: dir, st: st}
	mgr, err := jobs.NewManager(st, 2, func(_ string, done, _ float64, _ <-chan struct{}) {
		hv.mu.Lock()
		hv.steps = append(hv.steps, done)
		hv.mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	hv.mgr = mgr
	hv.h = New(&Deps{Store: st, Jobs: mgr})
	return hv
}

func (hv *harness) stepCount() int {
	hv.mu.Lock()
	defer hv.mu.Unlock()
	return len(hv.steps)
}

func (hv *harness) do(method, path, body string) (int, map[string]any) {
	hv.t.Helper()
	var rdr *bytes.Buffer
	if body != "" {
		rdr = bytes.NewBufferString(body)
	} else {
		rdr = bytes.NewBuffer(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	hv.h.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			hv.t.Fatalf("non-JSON response %d: %s", rec.Code, rec.Body.String())
		}
	}
	return rec.Code, out
}

// --- request builders ---

func rectReach(length, b, n, s float64) string {
	return fmt.Sprintf(
		`{"length":%g,"section":{"bottom_width":%g,"side_slope":0},"roughness":%g,"slope":%g}`,
		length, b, n, s)
}

func weirCtrl(crest float64) string {
	return fmt.Sprintf(`{"kind":"weir","width":3,"crest_height":%g,"discharge_coefficient":0.62}`, crest)
}

func depthCtrl(d float64) string {
	return fmt.Sprintf(`{"kind":"depth","depth":%g}`, d)
}

func caseIBody(crest float64) string {
	return fmt.Sprintf(`{"reaches":[%s],"design_flow":5,"control":%s}`,
		rectReach(6000, 3, 0.015, 0.001), weirCtrl(crest))
}

func createChannel(t *testing.T, hv *harness, body string) string {
	t.Helper()
	status, out := hv.do(http.MethodPost, "/v1/channels", body)
	if status != http.StatusCreated {
		t.Fatalf("create channel: %d %v", status, out)
	}
	return out["channel_id"].(string)
}

func submitAndWait(t *testing.T, hv *harness, id string, version int, body string) map[string]any {
	t.Helper()
	path := fmt.Sprintf("/v1/channels/%s/versions/%d/profiles", id, version)
	status, out := hv.do(http.MethodPost, path, body)
	if status != http.StatusAccepted && status != http.StatusOK {
		t.Fatalf("submit: %d %v", status, out)
	}
	jobID := out["job_id"].(string)
	return waitJob(t, hv, jobID)
}

func waitJob(t *testing.T, hv *harness, jobID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, out := hv.do(http.MethodGet, "/v1/jobs/"+jobID, "")
		if status != http.StatusOK {
			t.Fatalf("get job: %d %v", status, out)
		}
		switch out["status"] {
		case "succeeded", "failed", "cancelled", "interrupted":
			return out
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %s never finished", jobID)
	return nil
}

func profileOf(out map[string]any) map[string]any {
	res, ok := out["result"].(map[string]any)
	if !ok {
		return nil
	}
	return res["profile"].(map[string]any)
}

func depths(out map[string]any) []map[string]any {
	p := profileOf(out)
	raw := p["points"].([]any)
	pts := make([]map[string]any, len(raw))
	for i, r := range raw {
		pts[i] = r.(map[string]any)
	}
	return pts
}

func depthAt(out map[string]any, s float64) float64 {
	pts := depths(out)
	for i, pt := range pts {
		sd := pt["distance"].(float64)
		y := pt["depth"].(float64)
		if math.Abs(sd-s) < 1e-6 {
			return y
		}
		if i > 0 {
			pa := pts[i-1]
			sa := pa["distance"].(float64)
			if s >= sa && s <= sd {
				ya := pa["depth"].(float64)
				f := (s - sa) / (sd - sa)
				return ya + f*(y-ya)
			}
		}
	}
	return pts[len(pts)-1]["depth"].(float64)
}

// --- tests ---

// TestAPIProfileVersionLinkAndStaleness: results are attached to a version and
// report staleness after a new version is created.
func TestAPIProfileVersionLinkAndStaleness(t *testing.T) {
	hv := newHarness(t)
	id := createChannel(t, hv, caseIBody(1.0))

	out := submitAndWait(t, hv, id, 1, `{"mode":"full"}`)
	if out["status"] != "succeeded" {
		t.Fatalf("job: %v", out)
	}
	if out["version"].(float64) != 1 {
		t.Errorf("result not linked to version 1: %v", out["version"])
	}
	p := profileOf(out)
	if p["design_flow"].(float64) != 5 {
		t.Errorf("profile design flow = %v", p["design_flow"])
	}
	if out["stale"].(bool) {
		t.Error("fresh result must not be stale")
	}
	if math.Abs(depthAt(out, 0)-1.939) > 0.01 {
		t.Errorf("weir depth = %.4f", depthAt(out, 0))
	}

	// Profile endpoint shows the same result and links the job.
	status, got := hv.do(http.MethodGet,
		fmt.Sprintf("/v1/channels/%s/versions/1/profile", id), "")
	if status != http.StatusOK || got["job_id"] != out["job_id"] || got["version"].(float64) != 1 {
		t.Fatalf("profile endpoint: %d %v", status, got)
	}

	// Create v2 and check staleness flags.
	status, upd := hv.do(http.MethodPut, fmt.Sprintf("/v1/channels/%s", id),
		fmt.Sprintf(`{"base_version":1,%s`, stripBrace(caseIBody(1.2))))
	if status != http.StatusOK || upd["version"].(float64) != 2 {
		t.Fatalf("update: %d %v", status, upd)
	}
	status, got = hv.do(http.MethodGet,
		fmt.Sprintf("/v1/channels/%s/versions/1/profile", id), "")
	if status != http.StatusOK || got["stale"] != true {
		t.Fatalf("old result should be stale: %d %v", status, got)
	}
}

// stripBrace turns {"...":..} into "...":.. so it can be embedded after a
// leading field in a JSON object literal.
func stripBrace(body string) string {
	if len(body) > 0 && body[0] == '{' {
		return body[1:]
	}
	return body
}

// TestAPIPinnedValues is case I pinned through the public API.
func TestAPIPinnedValues(t *testing.T) {
	hv := newHarness(t)
	id := createChannel(t, hv, caseIBody(1.0))
	out := submitAndWait(t, hv, id, 1, "")
	if out["status"] != "succeeded" {
		t.Fatalf("%v", out)
	}
	for _, ref := range [][2]float64{{0, 1.939}, {1000, 1.276}, {2000, 1.090}} {
		if y := depthAt(out, ref[0]); math.Abs(y-ref[1]) > 0.01 {
			t.Errorf("depth at %.0f = %.4f, want %.3f +/- 0.01", ref[0], y, ref[1])
		}
	}
	// Output carries velocity and Froude number at every point.
	for _, pt := range depths(out) {
		if pt["velocity"].(float64) <= 0 {
			t.Errorf("non-positive velocity at %v", pt["distance"])
		}
		if pt["froude_number"].(float64) >= 1 {
			t.Errorf("Fr %.4f not subcritical at %v", pt["froude_number"], pt["distance"])
		}
	}
}

// TestAPIUniformAndDrawdown covers cases II and III via the API.
func TestAPIUniformAndDrawdown(t *testing.T) {
	hv := newHarness(t)

	// Need yn for the case-I reach: ask the legacy uniform-flow endpoint.
	status, uf := hv.do(http.MethodPost, "/v1/uniform-flow",
		`{"bottom_width":3,"side_slope":0,"roughness":0.015,"slope":0.001,"flow":5}`)
	if status != http.StatusOK {
		t.Fatalf("uniform flow: %d %v", status, uf)
	}
	yn := uf["normal_depth"].(float64)
	yc := 0.6566634

	body := fmt.Sprintf(`{"reaches":[%s],"design_flow":5,"control":%s}`,
		rectReach(6000, 3, 0.015, 0.001), depthCtrl(yn))
	id := createChannel(t, hv, body)
	out := submitAndWait(t, hv, id, 1, `{"mode":"full"}`)
	for _, pt := range depths(out) {
		if math.Abs(pt["depth"].(float64)-yn) > 1e-7 {
			t.Fatalf("case II: depth %.9f != yn %.9f at %v", pt["depth"], yn, pt["distance"])
		}
	}

	// Case III: downstream depth between yc and yn.
	mid := yc + 0.5*(yn-yc)
	body3 := fmt.Sprintf(`{"reaches":[%s],"design_flow":5,"control":%s}`,
		rectReach(8000, 3, 0.015, 0.001), depthCtrl(mid))
	id3 := createChannel(t, hv, body3)
	out3 := submitAndWait(t, hv, id3, 1, `{"mode":"full"}`)
	pts := depths(out3)
	prev := yc
	for _, pt := range pts {
		y := pt["depth"].(float64)
		if y < prev-1e-9 || y > yn+1e-9 {
			t.Fatalf("case III violated at %v: y %.6f, prev %.6f, yn %.6f", pt["distance"], y, prev, yn)
		}
		prev = y
	}
	reaches := profileOf(out3)["reaches"].([]any)
	if reaches[0].(map[string]any)["curve"] != "drawdown" {
		t.Errorf("case III curve label: %v", reaches[0])
	}
}

// TestAPIRaisingCrest is case IV: raising only the weir crest never lowers a
// depth anywhere.
func TestAPIRaisingCrest(t *testing.T) {
	hv := newHarness(t)
	idLow := createChannel(t, hv, caseIBody(1.0))
	outLow := submitAndWait(t, hv, idLow, 1, "")
	idHigh := createChannel(t, hv, caseIBody(1.2))
	outHigh := submitAndWait(t, hv, idHigh, 1, "")
	for _, pt := range depths(outLow) {
		s := pt["distance"].(float64)
		if yH := depthAt(outHigh, s); yH < pt["depth"].(float64)-1e-9 {
			t.Fatalf("depth lowered at %.0f: %.6f -> %.6f", s, pt["depth"], yH)
		}
	}
}

// TestAPITwoReachRoughnessChange is case V: changing only the upstream reach
// roughness leaves the downstream reach identical, and partial matches full.
func TestAPITwoReachRoughnessChange(t *testing.T) {
	hv := newHarness(t)
	mk := func(n float64) string {
		return fmt.Sprintf(`{"reaches":[%s,%s],"design_flow":5,"control":%s}`,
			rectReach(2000, 3, n, 0.001),
			rectReach(3000, 3, 0.015, 0.001),
			weirCtrl(1.0))
	}
	id := createChannel(t, hv, mk(0.015))
	v1 := submitAndWait(t, hv, id, 1, `{"mode":"full"}`)

	status, upd := hv.do(http.MethodPut, fmt.Sprintf("/v1/channels/%s", id),
		fmt.Sprintf(`{"base_version":1,%s`, stripBrace(mk(0.020))))
	if status != http.StatusOK {
		t.Fatalf("update: %d %v", status, upd)
	}

	v2partial := submitAndWait(t, hv, id, 2, `{"mode":"partial"}`)
	if v2partial["status"] != "succeeded" {
		t.Fatalf("%v", v2partial)
	}
	if rm := v2partial["result"].(map[string]any)["recompute_mode"]; rm != "partial" {
		t.Errorf("recompute_mode = %v", rm)
	}
	// Downstream reach (distance < 2000) bit-identical to v1.
	for _, pt := range depths(v1) {
		s := pt["distance"].(float64)
		if s < 2000 {
			if y2 := depthAt(v2partial, s); y2 != pt["depth"].(float64) {
				t.Fatalf("downstream reach changed at %.0f: %.12f != %.12f", s, y2, pt["depth"])
			}
		}
	}
	// Comparison summary present and points into the changed prefix.
	comp := v2partial["result"].(map[string]any)["comparison"].(map[string]any)
	if comp["max_delta_distance"].(float64) < 2000 {
		t.Errorf("max delta located in unchanged suffix: %v", comp["max_delta_distance"])
	}

	// Force full rerun and compare against the partial result.
	status, sub := hv.do(http.MethodPost,
		fmt.Sprintf("/v1/channels/%s/versions/2/profiles", id),
		`{"mode":"full","force":true}`)
	if status != http.StatusAccepted {
		t.Fatalf("force submit: %d %v", status, sub)
	}
	v2full := waitJob(t, hv, sub["job_id"].(string))
	var maxErr float64
	for _, pf := range depths(v2full) {
		s := pf["distance"].(float64)
		if e := math.Abs(depthAt(v2partial, s) - pf["depth"].(float64)); e > maxErr {
			maxErr = e
		}
	}
	if maxErr > 1e-9 {
		t.Errorf("partial vs full over API differ by %.2e", maxErr)
	}
}

// TestAPISteepFailure is case VI over the API: explicit failed status and the
// reach/distance are reported.
func TestAPISteepFailure(t *testing.T) {
	hv := newHarness(t)
	body := fmt.Sprintf(`{"reaches":[%s,%s],"design_flow":5,"control":%s}`,
		rectReach(2000, 3, 0.015, 0.05),
		rectReach(2000, 3, 0.015, 0.001),
		depthCtrl(1.5))
	id := createChannel(t, hv, body)
	out := submitAndWait(t, hv, id, 1, "")
	if out["status"] != "failed" {
		t.Fatalf("status = %v", out["status"])
	}
	fail := out["failure"].(map[string]any)
	if fail["kind"] != "steep_reach" {
		t.Errorf("failure kind = %v", fail["kind"])
	}
	if fail["reach"].(float64) != 0 || fail["distance"].(float64) != 2000 {
		t.Errorf("failure location = reach %v distance %v", fail["reach"], fail["distance"])
	}
	if out["result"] != nil {
		t.Error("failed job must not carry a result")
	}
	// Failed versions may be resubmitted after the definition is fixed.
	fixed := fmt.Sprintf(`{"base_version":1,%s`,
		stripBrace(fmt.Sprintf(`{"reaches":[%s,%s],"design_flow":5,"control":%s}`,
			rectReach(2000, 3, 0.015, 0.001),
			rectReach(2000, 3, 0.015, 0.001),
			depthCtrl(1.5))))
	if st, upd := hv.do(http.MethodPut, fmt.Sprintf("/v1/channels/%s", id), fixed); st != http.StatusOK {
		t.Fatalf("fix update: %d %v", st, upd)
	}
	out2 := submitAndWait(t, hv, id, 2, "")
	if out2["status"] != "succeeded" {
		t.Fatalf("fixed channel still fails: %v", out2)
	}
}

// TestAPIOptimisticConflict is case VII's stale-edit rejection over HTTP.
func TestAPIOptimisticConflict(t *testing.T) {
	hv := newHarness(t)
	id := createChannel(t, hv, caseIBody(1.0))
	goodBody := fmt.Sprintf(`{"base_version":1,%s`, stripBrace(caseIBody(1.1)))
	if st, out := hv.do(http.MethodPut, fmt.Sprintf("/v1/channels/%s", id), goodBody); st != http.StatusOK {
		t.Fatalf("first update: %d %v", st, out)
	}
	staleBody := fmt.Sprintf(`{"base_version":1,%s`, stripBrace(caseIBody(1.2)))
	st, out := hv.do(http.MethodPut, fmt.Sprintf("/v1/channels/%s", id), staleBody)
	if st != http.StatusConflict {
		t.Fatalf("stale update status = %d, want 409, body %v", st, out)
	}
	// History still has exactly two versions and the stale edit is absent.
	st, hist := hv.do(http.MethodGet, fmt.Sprintf("/v1/channels/%s/versions", id), "")
	if st != http.StatusOK || len(hist["versions"].([]any)) != 2 {
		t.Fatalf("history after conflict: %d %v", st, hist)
	}
}

// TestAPIPersistenceAcrossRestart covers case VII: close and reopen a manager
// on the same data directory; channels, history and results survive, and a
// job that was running is marked interrupted and can be resubmitted.
func TestAPIPersistenceAcrossRestart(t *testing.T) {
	hv := newHarness(t)
	id := createChannel(t, hv, caseIBody(1.0))
	out := submitAndWait(t, hv, id, 1, "")
	jobID := out["job_id"].(string)

	// Simulate a process death with a running job.
	if err := hv.st.CreateJob("orphan", id, 1, "full"); err != nil {
		t.Fatal(err)
	}
	if _, err := hv.st.MarkRunning("orphan"); err != nil {
		t.Fatal(err)
	}
	hv.mgr.Close()
	if err := hv.st.Close(); err != nil {
		t.Fatal(err)
	}

	// Fresh process, same mounted data directory.
	st2, err := store.Open(filepath.Join(hv.dir, "openchannel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st2.Close() })
	mgr2, err := jobs.NewManager(st2, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr2.Close)
	h2 := New(&Deps{Store: st2, Jobs: mgr2})

	rec := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v1/channels/%s/versions", id), nil)
	w := httptest.NewRecorder()
	h2.ServeHTTP(w, rec)
	var hist map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &hist)
	if w.Code != http.StatusOK || len(hist["versions"].([]any)) != 1 {
		t.Fatalf("history after restart: %d %v", w.Code, hist)
	}

	// Finished result still readable.
	rec = httptest.NewRequest(http.MethodGet, "/v1/jobs/"+jobID, nil)
	w = httptest.NewRecorder()
	h2.ServeHTTP(w, rec)
	var doneJob map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &doneJob)
	if doneJob["status"] != "succeeded" || doneJob["result"] == nil {
		t.Fatalf("finished job after restart: %v", doneJob)
	}

	// Orphan is interrupted, not stuck.
	rec = httptest.NewRequest(http.MethodGet, "/v1/jobs/orphan", nil)
	w = httptest.NewRecorder()
	h2.ServeHTTP(w, rec)
	var orphan map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &doneJob)
	_ = json.Unmarshal(w.Body.Bytes(), &orphan)
	if orphan["status"] != "interrupted" {
		t.Fatalf("orphan status = %v, want interrupted", orphan["status"])
	}

	// Resubmit runs.
	rec = httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/v1/channels/%s/versions/1/profiles", id),
		bytes.NewBufferString(`{"mode":"full"}`))
	rec.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h2.ServeHTTP(w, rec)
	var sub map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &sub)
	if w.Code != http.StatusAccepted {
		t.Fatalf("resubmit after restart: %d %v", w.Code, sub)
	}

	hv2 := &harness{t: t, st: st2, mgr: mgr2, h: h2, dir: hv.dir}
	fin := waitJob(t, hv2, sub["job_id"].(string))
	if fin["status"] != "succeeded" {
		t.Fatalf("resubmitted job: %v", fin)
	}
}

// TestAPICancelInFlight is case VIII over HTTP: progress advances, cancel
// takes effect, no result is ever written.
func TestAPICancelInFlight(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	st, err := store.Open(filepath.Join(dir, "openchannel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	block := make(chan struct{})
	blocked := make(chan struct{})
	var once sync.Once
	var stepMu sync.Mutex
	var steps int

	mgr, err := jobs.NewManager(st, 1, func(_ string, done, _ float64, _ <-chan struct{}) {
		stepMu.Lock()
		steps++
		stepMu.Unlock()
		if done >= 1500 {
			once.Do(func() { close(blocked) })
			<-block
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	h := New(&Deps{Store: st, Jobs: mgr})

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	createBody := []byte(caseIBody(1.0))
	rc, _ := http.Post(srv.URL+"/v1/channels", "application/json", bytes.NewReader(createBody))
	if rc.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", rc.StatusCode)
	}
	// Re-read to get the id (simplest: list channels).
	rl, _ := http.Get(srv.URL + "/v1/channels")
	var listing struct {
		Channels []string `json:"channels"`
	}
	_ = json.NewDecoder(rl.Body).Decode(&listing)
	_ = rl.Body.Close()
	id := listing.Channels[0]

	rs, _ := http.Post(srv.URL+fmt.Sprintf("/v1/channels/%s/versions/1/profiles", id),
		"application/json", bytes.NewReader([]byte(`{"mode":"full"}`)))
	var sub map[string]any
	_ = json.NewDecoder(rs.Body).Decode(&sub)
	_ = rs.Body.Close()
	if rs.StatusCode != http.StatusAccepted {
		t.Fatalf("submit %d %v", rs.StatusCode, sub)
	}
	jobID := sub["job_id"].(string)

	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never parked")
	}

	// Observe advancing progress.
	var sawProgress bool
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rg, _ := http.Get(srv.URL + "/v1/jobs/" + jobID)
		var jb map[string]any
		_ = json.NewDecoder(rg.Body).Decode(&jb)
		_ = rg.Body.Close()
		if p := jb["progress"].(float64); p > 0.1 && p < 0.5 {
			sawProgress = true
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !sawProgress {
		t.Fatal("never observed in-flight progress")
	}
	stepMu.Lock()
	stepsBefore := steps
	stepMu.Unlock()

	rq, _ := http.Post(srv.URL+"/v1/jobs/"+jobID+"/cancel", "application/json", nil)
	var cancelled struct {
		Cancelled bool `json:"cancelled"`
	}
	_ = json.NewDecoder(rq.Body).Decode(&cancelled)
	_ = rq.Body.Close()
	if !cancelled.Cancelled {
		t.Fatal("cancel returned cancelled=false")
	}
	close(block)

	// Wait for terminal cancelled and assert no result.
	for i := 0; i < 200; i++ {
		rg, _ := http.Get(srv.URL + "/v1/jobs/" + jobID)
		var jb map[string]any
		_ = json.NewDecoder(rg.Body).Decode(&jb)
		_ = rg.Body.Close()
		if jb["status"] == "cancelled" {
			if jb["result"] != nil {
				t.Fatalf("cancelled job carries a result: %v", jb["result"])
			}
			break
		}
		if i == 199 {
			t.Fatalf("job did not cancel: %v", jb["status"])
		}
		time.Sleep(10 * time.Millisecond)
	}

	time.Sleep(50 * time.Millisecond)
	stepMu.Lock()
	stepsAfter := steps
	stepMu.Unlock()
	if stepsAfter > stepsBefore+1 {
		t.Errorf("worker continued after cancel: %d -> %d steps", stepsBefore, stepsAfter)
	}
}

// TestAPISubmitBodies checks submit edge cases: empty body is accepted and
// defaults to partial (which for v1 falls back to full), unknown mode is 400.
func TestAPISubmitBodies(t *testing.T) {
	hv := newHarness(t)
	id := createChannel(t, hv, caseIBody(1.0))

	// Empty body.
	path := fmt.Sprintf("/v1/channels/%s/versions/1/profiles", id)
	rec := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(nil))
	rec.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	hv.h.ServeHTTP(w, rec)
	if w.Code != http.StatusAccepted && w.Code != http.StatusOK {
		t.Fatalf("empty body submit: %d %s", w.Code, w.Body.String())
	}
	var sub map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &sub); err != nil {
		t.Fatal(err)
	}
	out := waitJob(t, hv, sub["job_id"].(string))
	if out["status"] != "succeeded" {
		t.Fatalf("empty-body job: %v", out["status"])
	}
	// No base profile exists yet, so a "partial" request honestly records
	// that a full integration was performed.
	if rm := out["result"].(map[string]any)["recompute_mode"]; rm != "full" {
		t.Errorf("recompute_mode = %v, want full (no base profile)", rm)
	}

	// Unknown mode -> 400.
	if st, bad := hv.do(http.MethodPost, path, `{"mode":"turbo"}`); st != http.StatusBadRequest {
		t.Fatalf("bad mode: %d %v", st, bad)
	}

	// Unknown version -> 404.
	if st, _ := hv.do(http.MethodPost,
		fmt.Sprintf("/v1/channels/%s/versions/7/profiles", id), `{}`); st != http.StatusNotFound {
		t.Fatalf("unknown version submit = %d, want 404", st)
	}

	// Unknown job -> 404.
	if st, _ := hv.do(http.MethodGet, "/v1/jobs/nope", ""); st != http.StatusNotFound {
		t.Fatalf("unknown job = %d, want 404", st)
	}
}

// TestAPIValidationErrors checks that bad canal definitions are rejected with
// 400 and a field name.
func TestAPIValidationErrors(t *testing.T) {
	hv := newHarness(t)
	cases := []struct {
		body  string
		field string
	}{
		{`{"reaches":[],"design_flow":5,"control":{"kind":"depth","depth":2}}`, "reaches"},
		{fmt.Sprintf(`{"reaches":[%s],"design_flow":0,"control":%s}`,
			rectReach(1000, 3, 0.015, 0.001), depthCtrl(2)), "design_flow"},
		{fmt.Sprintf(`{"reaches":[%s],"design_flow":5,"control":{"kind":"weir","width":0,"crest_height":1,"discharge_coefficient":0.62}}`,
			rectReach(1000, 3, 0.015, 0.001)), "control.width"},
		{fmt.Sprintf(`{"reaches":[%s],"design_flow":5,"control":{"kind":"mystery"}}`,
			rectReach(1000, 3, 0.015, 0.001)), "control.kind"},
	}
	for _, tc := range cases {
		st, out := hv.do(http.MethodPost, "/v1/channels", tc.body)
		if st != http.StatusBadRequest || out["field"] != tc.field {
			t.Errorf("body %s -> %d %v, want 400 field %q", tc.body, st, out, tc.field)
		}
	}
}
