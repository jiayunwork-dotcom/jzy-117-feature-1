package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"openchannel/internal/channel"
	"openchannel/internal/geometry"
	"openchannel/internal/jobs"
	"openchannel/internal/profile"
	"openchannel/internal/store"
	"openchannel/internal/validation"
)

type api struct {
	deps *Deps
}

// --- request / response shapes ---

type sectionReq struct {
	BottomWidth float64 `json:"bottom_width"`
	SideSlope   float64 `json:"side_slope"`
}

type reachReq struct {
	Name      string     `json:"name"`
	Length    float64    `json:"length"`
	Section   sectionReq `json:"section"`
	Roughness float64    `json:"roughness"`
	Slope     float64    `json:"slope"`
}

type controlReq struct {
	Kind        string  `json:"kind"`
	Width       float64 `json:"width"`
	CrestHeight float64 `json:"crest_height"`
	Cd          float64 `json:"discharge_coefficient"`
	Depth       float64 `json:"depth"`
}

type definitionReq struct {
	Reaches    []reachReq `json:"reaches"`
	DesignFlow float64    `json:"design_flow"`
	Control    controlReq `json:"control"`
}

type createChannelRequest struct {
	definitionReq
}

type updateChannelRequest struct {
	BaseVersion int `json:"base_version"`
	definitionReq
}

func (r definitionReq) toDefinition() channel.Definition {
	d := channel.Definition{
		DesignFlow: r.DesignFlow,
		Control: channel.Control{
			Kind:        channel.ControlKind(r.Control.Kind),
			Width:       r.Control.Width,
			CrestHeight: r.Control.CrestHeight,
			Cd:          r.Control.Cd,
			Depth:       r.Control.Depth,
		},
	}
	for _, rc := range r.Reaches {
		d.Reaches = append(d.Reaches, channel.Reach{
			Name:      rc.Name,
			Length:    rc.Length,
			Roughness: rc.Roughness,
			Slope:     rc.Slope,
			Section:   geometrySection(rc.Section),
		})
	}
	return d
}

type versionResponse struct {
	ChannelID  string             `json:"channel_id"`
	Version    int                `json:"version"`
	CreatedAt  string             `json:"created_at"`
	Stale      bool               `json:"stale"`
	Definition channel.Definition `json:"definition"`
}

func (a *api) createChannel(w http.ResponseWriter, r *http.Request) {
	var req createChannelRequest
	if !decode(w, r, &req) {
		return
	}
	def := req.toDefinition()
	if err := def.Validate(); err != nil {
		writeValidationError(w, err)
		return
	}
	id := newExternalID()
	if err := a.deps.Store.CreateChannel(id, def); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"channel_id": id,
		"version":    1,
	})
}

func (a *api) listChannels(w http.ResponseWriter, r *http.Request) {
	ids, err := a.deps.Store.ListChannels()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if ids == nil {
		ids = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": ids})
}

func (a *api) getChannel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	latest, err := a.deps.Store.LatestVersion(id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	a.writeVersion(w, id, latest)
}

func (a *api) updateChannel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req updateChannelRequest
	if !decode(w, r, &req) {
		return
	}
	if req.BaseVersion <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "base_version is required and must be positive",
			"field": "base_version",
		})
		return
	}
	def := req.toDefinition()
	if err := def.Validate(); err != nil {
		writeValidationError(w, err)
		return
	}
	newVersion, err := a.deps.Store.UpdateChannel(id, req.BaseVersion, def)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"channel_id":   id,
		"version":      newVersion,
		"base_version": req.BaseVersion,
	})
}

func (a *api) listVersions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	infos, err := a.deps.Store.ListVersions(id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": infos})
}

func (a *api) getVersion(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	v, ok := parseVersion(w, r)
	if !ok {
		return
	}
	a.writeVersion(w, id, v)
}

func (a *api) writeVersion(w http.ResponseWriter, id string, version int) {
	def, err := a.deps.Store.GetDefinition(id, version)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	latest, err := a.deps.Store.LatestVersion(id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	created := ""
	if infos, err := a.deps.Store.ListVersions(id); err == nil {
		for _, info := range infos {
			if info.Version == version {
				created = info.CreatedAt
			}
		}
	}
	writeJSON(w, http.StatusOK, versionResponse{
		ChannelID:  id,
		Version:    version,
		CreatedAt:  created,
		Stale:      version < latest,
		Definition: def,
	})
}

// --- profiles / jobs ---

type submitProfileRequest struct {
	Mode  string `json:"mode"`
	Force bool   `json:"force"`
}

func (a *api) submitProfile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	version, ok := parseVersion(w, r)
	if !ok {
		return
	}
	var req submitProfileRequest
	// An empty body means "default mode, no force"; anything else must parse.
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON request body"})
			return
		}
	}
	mode := req.Mode
	if mode == "" {
		mode = profile.ModePartial
	}
	jobID, reused, err := a.deps.Jobs.Submit(r.Context(), id, version, mode, req.Force)
	if err != nil {
		if errors.Is(err, jobs.ErrInvalidMode) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "field": "mode"})
			return
		}
		writeStoreError(w, err)
		return
	}
	status := http.StatusAccepted
	if reused {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{
		"job_id":     jobID,
		"channel_id": id,
		"version":    version,
		"reused":     reused,
	})
}

func (a *api) getJob(w http.ResponseWriter, r *http.Request) {
	job, err := a.deps.Store.GetJob(r.PathValue("jobID"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	latest, err := a.deps.Store.LatestVersion(job.ChannelID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	resp := map[string]any{
		"job_id":     job.ID,
		"channel_id": job.ChannelID,
		"version":    job.Version,
		"status":     job.Status,
		"progress":   job.Progress,
		"recompute":  job.Recompute,
		"stale":      job.Version < latest,
		"created_at": job.CreatedAt,
		"updated_at": job.UpdatedAt,
	}
	if job.StartedAt != "" {
		resp["started_at"] = job.StartedAt
	}
	if job.FinishedAt != "" {
		resp["finished_at"] = job.FinishedAt
	}
	if job.Failure != nil {
		resp["failure"] = job.Failure
	}
	if job.Error != "" {
		resp["error"] = job.Error
	}
	if job.Profile != nil {
		resp["result"] = job.Profile
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *api) cancelJob(w http.ResponseWriter, r *http.Request) {
	ok, err := a.deps.Jobs.Cancel(r.PathValue("jobID"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": ok})
}

func (a *api) getProfile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	version, ok := parseVersion(w, r)
	if !ok {
		return
	}
	job, err := a.deps.Store.LatestJobForVersion(id, version)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"error": "no profile computation has been submitted for this version",
			})
			return
		}
		writeStoreError(w, err)
		return
	}
	latest, err := a.deps.Store.LatestVersion(id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	resp := map[string]any{
		"channel_id": id,
		"version":    version,
		"stale":      version < latest,
		"job_id":     job.ID,
		"status":     job.Status,
	}
	if job.Failure != nil {
		resp["failure"] = job.Failure
	}
	if job.Profile != nil {
		resp["result"] = job.Profile
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- helpers ---

func parseVersion(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.PathValue("version")
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "version must be a positive integer",
			"field": "version",
		})
		return 0, false
	}
	return v, true
}

func writeValidationError(w http.ResponseWriter, err error) {
	var ve *validation.Error
	if errors.As(err, &ve) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": ve.Message,
			"field": ve.Field,
		})
		return
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
	case errors.Is(err, store.ErrVersionConflict):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
	case errors.Is(err, store.ErrActiveJob):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}
}

func geometrySection(s sectionReq) geometry.Section {
	return geometry.Section{BottomWidth: s.BottomWidth, SideSlope: s.SideSlope}
}

func newExternalID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
