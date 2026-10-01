// Package server wires the hydraulic packages to net/http. All request
// validation, iteration and formulas stay in their own packages; this package
// only translates JSON.
package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"openchannel/internal/flow"
	"openchannel/internal/geometry"
	"openchannel/internal/jobs"
	"openchannel/internal/manning"
	"openchannel/internal/store"
	"openchannel/internal/validation"
	"openchannel/internal/weir"
)

// Deps are the persistent dependencies of the canal-line API. The two legacy
// hydraulic endpoints do not need them, so New(nil) keeps the original
// behaviour and the legacy tests' wiring.
type Deps struct {
	Store *store.Store
	Jobs  *jobs.Manager
}

// New builds the HTTP handler with all routes registered.
func New(deps ...*Deps) http.Handler {
	var d *Deps
	if len(deps) > 0 {
		d = deps[0]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", health)
	mux.HandleFunc("POST /v1/uniform-flow", uniformFlow)
	mux.HandleFunc("POST /v1/weir-flow", weirFlow)
	if d != nil && d.Store != nil {
		api := &api{deps: d}
		mux.HandleFunc("POST /v1/channels", api.createChannel)
		mux.HandleFunc("GET /v1/channels", api.listChannels)
		mux.HandleFunc("GET /v1/channels/{id}", api.getChannel)
		mux.HandleFunc("PUT /v1/channels/{id}", api.updateChannel)
		mux.HandleFunc("GET /v1/channels/{id}/versions", api.listVersions)
		mux.HandleFunc("GET /v1/channels/{id}/versions/{version}", api.getVersion)
		mux.HandleFunc("GET /v1/channels/{id}/versions/{version}/profile", api.getProfile)
		mux.HandleFunc("POST /v1/channels/{id}/versions/{version}/profiles", api.submitProfile)
		mux.HandleFunc("GET /v1/jobs/{jobID}", api.getJob)
		mux.HandleFunc("POST /v1/jobs/{jobID}/cancel", api.cancelJob)
	}
	return mux
}

func health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- uniform open-channel flow ---

type uniformRequest struct {
	BottomWidth float64 `json:"bottom_width"` // metres, required > 0
	SideSlope   float64 `json:"side_slope"`   // m horizontal per vertical; 0 = rectangular
	Roughness   float64 `json:"roughness"`    // Manning's n (SI), > 0
	Slope       float64 `json:"slope"`        // channel-bottom slope S0, > 0
	Flow        float64 `json:"flow"`         // discharge Q in m^3/s, >= 0
}

type uniformResponse struct {
	NormalDepth     float64 `json:"normal_depth"`     // metres
	Velocity        float64 `json:"velocity"`         // m/s
	FroudeNumber    float64 `json:"froude_number"`    // -
	Regime          string  `json:"regime"`           // subcritical / critical / supercritical / no_flow
	HydraulicRadius float64 `json:"hydraulic_radius"` // metres
	Area            float64 `json:"area"`             // m^2 (geometry the answers are derived from)
	TopWidth        float64 `json:"top_width"`        // metres (same geometry functions)
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

// --- rectangular sharp-crested weir ---

type weirRequest struct {
	Width float64  `json:"width"` // weir width b, metres, > 0
	Head  float64  `json:"head"`  // head H over the weir, metres, > 0
	Cd    *float64 `json:"discharge_coefficient,omitempty"`
}

type weirResponse struct {
	Flow                 float64 `json:"flow"`                  // m^3/s
	Width                float64 `json:"width"`                 // metres
	Head                 float64 `json:"head"`                  // metres
	DischargeCoefficient float64 `json:"discharge_coefficient"` // Cd actually used
	Note                 string  `json:"note,omitempty"`
}

// note explains that weir head and channel normal depth are not interchangeable.
// The service never silently couples or modifies either formula.
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

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
