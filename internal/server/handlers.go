package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"openchannel/internal/flow"
	"openchannel/internal/geometry"
	"openchannel/internal/manning"
	"openchannel/internal/profile"
	"openchannel/internal/runner"
	"openchannel/internal/store"
	"openchannel/internal/validation"
	"openchannel/internal/weir"
)

// --- uniform open-channel flow (legacy contract, unchanged) ---

type uniformRequest struct {
	BottomWidth float64 `json:"bottom_width"`
	SideSlope   float64 `json:"side_slope"`
	Roughness   float64 `json:"roughness"`
	Slope       float64 `json:"slope"`
	Flow        float64 `json:"flow"`
}

type uniformResponse struct {
	NormalDepth     float64 `json:"normal_depth"`
	Velocity        float64 `json:"velocity"`
	FroudeNumber    float64 `json:"froude_number"`
	Regime          string  `json:"regime"`
	HydraulicRadius float64 `json:"hydraulic_radius"`
	Area            float64 `json:"area"`
	TopWidth        float64 `json:"top_width"`
}

func uniformFlow(w http.ResponseWriter, r *http.Request) {
	var req uniformRequest
	if !decode(w, r, &req) {
		return
	}

	sec := geometry.Section{BottomWidth: req.BottomWidth, SideSlope: req.SideSlope}
	in := manning.Input{
		Section:   sec,
		Roughness: req.Roughness,
		Slope:     req.Slope,
		Flow:      req.Flow,
	}

	yn, err := manning.NormalDepth(in)
	if err != nil {
		writeHydraulicError(w, err)
		return
	}

	q := flow.AtDepth(sec, yn, in.Flow)
	writeJSON(w, http.StatusOK, uniformResponse{
		NormalDepth:     yn,
		Velocity:        q.Velocity,
		FroudeNumber:    q.Froude,
		Regime:          flow.Regime(q.Froude),
		HydraulicRadius: q.HydraulicRadius,
		Area:            q.Area,
		TopWidth:        q.TopWidth,
	})
}

// --- rectangular sharp-crested weir (legacy contract, unchanged) ---

type weirRequest struct {
	Width float64  `json:"width"`
	Head  float64  `json:"head"`
	Cd    *float64 `json:"discharge_coefficient,omitempty"`
}

type weirResponse struct {
	Flow                 float64 `json:"flow"`
	Width                float64 `json:"width"`
	Head                 float64 `json:"head"`
	DischargeCoefficient float64 `json:"discharge_coefficient"`
	Note                 string  `json:"note,omitempty"`
}

const note = "weir head H is referenced to the weir crest and is not a channel " +
	"normal depth; compare them only after converting to a common datum"

func weirFlow(w http.ResponseWriter, r *http.Request) {
	var req weirRequest
	if !decode(w, r, &req) {
		return
	}

	cd := weir.DefaultDischargeCoefficient
	if req.Cd != nil {
		cd = *req.Cd
	}

	q, err := weir.Discharge(weir.Input{
		Width:                req.Width,
		Head:                 req.Head,
		DischargeCoefficient: cd,
	})
	if err != nil {
		writeHydraulicError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, weirResponse{
		Flow:                 q,
		Width:                req.Width,
		Head:                 req.Head,
		DischargeCoefficient: cd,
		Note:                 note,
	})
}

// --- channels ---

// sectionRequest is the JSON shape of a trapezoidal/rectangular section.
type sectionRequest struct {
	BottomWidth float64 `json:"bottom_width"`
	SideSlope   float64 `json:"side_slope"` // 0 = rectangular
}

type reachRequest struct {
	Name      string         `json:"name"`
	Length    float64        `json:"length"`
	Section   sectionRequest `json:"section"`
	Roughness float64        `json:"roughness"`
	Slope     float64        `json:"slope"`
}

type weirControlRequest struct {
	Width                float64 `json:"width"`
	CrestHeight          float64 `json:"crest_height"`
	DischargeCoefficient float64 `json:"discharge_coefficient"`
}

type controlRequest struct {
	Type  string              `json:"type"`
	Weir  *weirControlRequest `json:"weir,omitempty"`
	Depth float64             `json:"depth,omitempty"`
}

type channelCreateRequest struct {
	Name    string         `json:"name"`
	Note    string         `json:"note"`
	Flow    float64        `json:"flow"`
	Reaches []reachRequest `json:"reaches"`
	Control controlRequest `json:"control"`
}

type channelUpdateRequest struct {
	BaseVersion int            `json:"base_version"` // optimistic-lock base, required
	Name        string         `json:"name"`         // optional rename
	Note        string         `json:"note"`
	Flow        float64        `json:"flow"`
	Reaches     []reachRequest `json:"reaches"`
	Control     controlRequest `json:"control"`
}

func toSpecReaches(reaches []reachRequest, flow float64, ctrl controlRequest) profile.Spec {
	spec := profile.Spec{Flow: flow}
	for _, r := range reaches {
		spec.Reaches = append(spec.Reaches, profile.ReachSpec{
			Name:      r.Name,
			Length:    r.Length,
			Section:   geometry.Section{BottomWidth: r.Section.BottomWidth, SideSlope: r.Section.SideSlope},
			Roughness: r.Roughness,
			Slope:     r.Slope,
		})
	}
	spec.Control = profile.Control{Type: ctrl.Type, Depth: ctrl.Depth}
	if ctrl.Weir != nil {
		cd := ctrl.Weir.DischargeCoefficient
		if cd == 0 {
			cd = weir.DefaultDischargeCoefficient
		}
		spec.Control.Weir = &profile.WeirControl{
			Width:                ctrl.Weir.Width,
			CrestHeight:          ctrl.Weir.CrestHeight,
			DischargeCoefficient: cd,
		}
	}
	return spec
}

func toSpec(req *channelCreateRequest) profile.Spec {
	return toSpecReaches(req.Reaches, req.Flow, req.Control)
}

func toSpecUpdate(req *channelUpdateRequest) profile.Spec {
	return toSpecReaches(req.Reaches, req.Flow, req.Control)
}

func validateSpec(spec profile.Spec) error { return spec.Validate() }

func (d *dependencies) createChannel(w http.ResponseWriter, r *http.Request) {
	var req channelCreateRequest
	if !decode(w, r, &req) {
		return
	}
	spec := toSpec(&req)
	if err := validateSpec(spec); err != nil {
		writeHydraulicError(w, err)
		return
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		writeServerError(w, err)
		return
	}
	ch, err := d.store.CreateChannel(newID(), req.Name, raw, req.Note)
	if err != nil {
		writeServerError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, channelResponse(ch))
}

func (d *dependencies) addVersion(w http.ResponseWriter, r *http.Request) {
	channelID := r.PathValue("channelID")
	var req channelUpdateRequest
	if !decode(w, r, &req) {
		return
	}
	if req.BaseVersion <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "base_version is required and must be positive",
			"field": "base_version",
		})
		return
	}
	spec := toSpecUpdate(&req)
	if err := validateSpec(spec); err != nil {
		writeHydraulicError(w, err)
		return
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		writeServerError(w, err)
		return
	}
	ch, err := d.store.AddVersion(channelID, req.BaseVersion, raw, req.Note)
	if errors.Is(err, store.ErrVersionConflict) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": err.Error(),
			"code":  "version_conflict",
		})
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeServerError(w, err)
		return
	}
	if req.Name != "" {
		if err := d.store.Rename(channelID, req.Name); err != nil {
			writeServerError(w, err)
			return
		}
		ch.Name = req.Name
	}
	writeJSON(w, http.StatusCreated, channelResponse(ch))
}

func (d *dependencies) listChannels(w http.ResponseWriter, r *http.Request) {
	channels, err := d.store.ListChannels()
	if err != nil {
		writeServerError(w, err)
		return
	}
	items := make([]map[string]any, 0, len(channels))
	for _, c := range channels {
		items = append(items, map[string]any{
			"id":              c.ID,
			"name":            c.Name,
			"created_at":      c.CreatedAt,
			"current_version": c.CurrentVersion(),
			"versions":        len(c.Versions),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": items})
}

func (d *dependencies) getChannel(w http.ResponseWriter, r *http.Request) {
	ch, err := d.store.GetChannel(r.PathValue("channelID"))
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeServerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, channelResponse(ch))
}

func (d *dependencies) getVersion(w http.ResponseWriter, r *http.Request) {
	channelID := r.PathValue("channelID")
	v, err := parseVersion(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ver, err := d.store.GetVersion(channelID, v)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeServerError(w, err)
		return
	}
	// Include staleness: succeeded result for this version is stale once a
	// newer version exists.
	ch, _ := d.store.GetChannel(channelID)
	stale := ch != nil && ch.CurrentVersion() > ver.Number
	var spec profile.Spec
	_ = json.Unmarshal(ver.Spec, &spec)
	writeJSON(w, http.StatusOK, map[string]any{
		"channel_id":   channelID,
		"number":       ver.Number,
		"created_at":   ver.CreatedAt,
		"note":         ver.Note,
		"spec":         spec,
		"is_current":   ch != nil && ch.CurrentVersion() == ver.Number,
		"result_stale": stale,
	})
}

func channelResponse(c *store.Channel) map[string]any {
	versions := make([]map[string]any, 0, len(c.Versions))
	for _, v := range c.Versions {
		var spec profile.Spec
		_ = json.Unmarshal(v.Spec, &spec)
		versions = append(versions, map[string]any{
			"number":     v.Number,
			"created_at": v.CreatedAt,
			"note":       v.Note,
			"spec":       spec,
		})
	}
	return map[string]any{
		"id":              c.ID,
		"name":            c.Name,
		"created_at":      c.CreatedAt,
		"current_version": c.CurrentVersion(),
		"versions":        versions,
	}
}

// --- profile jobs ---

func (d *dependencies) submitProfile(w http.ResponseWriter, r *http.Request) {
	channelID := r.PathValue("channelID")
	v, err := parseVersion(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if _, err := d.store.GetVersion(channelID, v); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		} else {
			writeServerError(w, err)
		}
		return
	}
	job, reused, reason, err := d.runner.Submit(channelID, v)
	if err != nil {
		writeServerError(w, err)
		return
	}
	status := http.StatusAccepted
	if reused {
		status = http.StatusOK
	}
	body := jobResponse(job)
	if reused {
		body["reused"] = true
		body["reason"] = reason
	}
	writeJSON(w, status, body)
}

func (d *dependencies) getJob(w http.ResponseWriter, r *http.Request) {
	job, err := d.store.GetJob(r.PathValue("jobID"))
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeServerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, jobResponse(job))
}

func (d *dependencies) cancelJob(w http.ResponseWriter, r *http.Request) {
	job, err := d.runner.Cancel(r.PathValue("jobID"))
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, runner.ErrJobNotActive) {
		writeJSON(w, http.StatusConflict, jobResponse(job))
		return
	}
	if err != nil {
		writeServerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, jobResponse(job))
}

func (d *dependencies) getResult(w http.ResponseWriter, r *http.Request) {
	channelID := r.PathValue("channelID")
	v, err := parseVersion(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	job, err := d.store.LatestSuccess(channelID, v)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "no succeeded profile result for this channel version yet",
		})
		return
	}
	if err != nil {
		writeServerError(w, err)
		return
	}
	var env map[string]any
	if err := json.Unmarshal(job.Result, &env); err != nil {
		writeServerError(w, err)
		return
	}
	env["stale"] = job.ResultStale
	env["job_id"] = job.ID
	writeJSON(w, http.StatusOK, env)
}

func jobResponse(j *store.Job) map[string]any {
	body := map[string]any{
		"id":           j.ID,
		"channel_id":   j.ChannelID,
		"version":      j.Version,
		"status":       j.Status,
		"progress":     j.Progress,
		"created_at":   j.CreatedAt,
		"updated_at":   j.UpdatedAt,
		"result_stale": j.ResultStale,
	}
	if j.Error != "" {
		body["error"] = j.Error
	}
	if len(j.Failure) > 0 {
		var f map[string]any
		if json.Unmarshal(j.Failure, &f) == nil {
			body["failure"] = f
		}
	}
	if j.StartedAt != nil {
		body["started_at"] = *j.StartedAt
	}
	if j.FinishedAt != nil {
		body["finished_at"] = *j.FinishedAt
	}
	if j.Status == store.JobSucceeded {
		body["result_url"] = "/v1/channels/" + j.ChannelID +
			"/versions/" + itoa(j.Version) + "/profile-result"
	}
	return body
}

// --- plumbing ---

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON request body"})
		return false
	}
	return true
}

func writeHydraulicError(w http.ResponseWriter, err error) {
	var ve *validation.Error
	if errors.As(err, &ve) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": ve.Message,
			"field": ve.Field,
		})
		return
	}
	if errors.Is(err, manning.ErrDidNotConverge) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

func writeServerError(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
