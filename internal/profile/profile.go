// Package profile computes steady, gradually varied flow (GVF) water-surface
// profiles along a canal line made of several prismatic reaches.
//
// # Method and assumptions
//
// The profile is integrated as a boundary-value problem marched UPSTREAM from
// the downstream control, which is the well-posed direction for subcritical
// (mild) flow. Between two neighbouring stations a downstream station "d" and
// the next station "u" one step upstream, with bed elevation rising by
// S0*dx, the energy balance is
//
//	z_b,u + y_u + V_u^2/(2g) = z_b,d + y_d + V_d^2/(2g) + Sf_mean*dx
//
// i.e. with specific energy E(y) = y + Q^2/(2g A(y)^2) and mean friction
// slope Sf_mean = (Sf(y_d) + Sf(y_u))/2:
//
//	E(y_u) = E(y_d) + (Sf_mean - S0) * dx
//
// This is the standard-step method with the IMPLICIT TRAPEZOIDAL friction
// term, which is A-stable for these equations and lets us take fixed spatial
// steps without overshooting the normal-depth asymptote. The implicit scalar
// equation in y_u is solved by bisection on the subcritical limb (y > yc):
// its residual is strictly monotone there because dE/dy = 1 - Fr^2 > 0 and
// Sf decreases with depth.
//
// Every hydraulic quantity is taken from the existing packages: geometry
// (A, P, T), flow (V, Fr), manning (Sf is expressed through the existing
// Manning discharge: Sf = S0*(Q/Q_manning(y))^2) and weir (the downstream
// stage comes from the channel package, which itself inverts the one weir
// law). There is no second copy of any formula.
//
// # Scope and failures
//
// Hydraulic jumps are deliberately not handled. A reach whose normal depth is
// at or below critical depth (critical/steep slope) fails immediately, and any
// step whose root would have to sit at or below yc - i.e. the profile would
// cross the critical depth - fails as well, naming the reach and the distance
// upstream of the control. No curve spanning critical depth is ever returned.
//
// Reaches join with continuous water depth and no local head loss (the
// standard GVF treatment for a continuous prismatic canal); bed elevation is
// continuous because each reach climbs S0*L.
package profile

import (
	"errors"
	"fmt"
	"math"

	"openchannel/internal/channel"
	"openchannel/internal/flow"
	"openchannel/internal/manning"
)

// gConst is the same SI gravity constant the flow package uses.
const gConst = 9.81

// DefaultStep is the target maximum spatial step, in metres. Each reach is
// split into ceil(L/DefaultStep) equal intervals so reach ends (and hence
// junctions and the downstream control) land exactly on grid points.
const DefaultStep = 50.0

// Curve labels for a reach.
const (
	CurveBackwater = "backwater" // y >= yn, depth grows downstream (M1, 壅水)
	CurveDrawdown  = "drawdown"  // y <= yn, depth falls downstream (M2, 降水)
	CurveUniform   = "uniform"   // y ~ yn along the reach
)

// Failure kinds.
const (
	FailSteepReach       = "steep_reach"       // a reach is steep (yn < yc)
	FailCriticalSlope    = "critical_slope"    // yn ~= yc, no unique subcritical profile
	FailCriticalCrossing = "critical_crossing" // a step would need y <= yc
	FailControlDepth     = "control_depth_bad" // downstream control is not subcritical
)

// ErrCancelled is returned when Hooks.Cancel is closed during Compute.
var ErrCancelled = errors.New("profile: computation cancelled")

// ComputeFailure names where and why the profile could not be produced.
type ComputeFailure struct {
	Kind     string  `json:"kind"`
	Reach    int     `json:"reach"`    // storage index (0 = most upstream)
	Distance float64 `json:"distance"` // metres upstream of the downstream control
	Message  string  `json:"message"`
}

func (f *ComputeFailure) Error() string {
	return fmt.Sprintf("profile: %s at reach %d, %.3f m upstream of control: %s",
		f.Kind, f.Reach, f.Distance, f.Message)
}

// Point is one profile station. Distance is measured upstream from the
// downstream control section (s = 0 at the control).
type Point struct {
	Distance float64 `json:"distance"` // m upstream of the control
	Reach    int     `json:"reach"`    // storage index, 0 = most upstream
	Depth    float64 `json:"depth"`    // m
	Velocity float64 `json:"velocity"` // m/s
	Froude   float64 `json:"froude_number"`
}

// ReachResult summarises one reach after integration.
type ReachResult struct {
	Index         int     `json:"index"` // storage index, 0 = most upstream
	Curve         string  `json:"curve"` // backwater / drawdown / uniform
	NormalDepth   float64 `json:"normal_depth"`
	CriticalDepth float64 `json:"critical_depth"`
	DepthUp       float64 `json:"depth_upstream_end"`
	DepthDown     float64 `json:"depth_downstream_end"`
}

// Profile is one finished water-surface profile. Points run from the
// downstream control (distance 0) upstream.
type Profile struct {
	Points              []Point       `json:"points"`
	Reaches             []ReachResult `json:"reaches"`
	DesignFlow          float64       `json:"design_flow"`
	DownstreamDepth     float64       `json:"downstream_depth"`
	TotalLength         float64       `json:"total_length"`
	StepTarget          float64       `json:"step_target_metres"`
	UpstreamNormalMatch bool          `json:"upstream_end_at_normal_depth"`
	UpstreamDepthError  float64       `json:"upstream_end_normal_depth_error"` // |y_up - yn|, m
}

// Hooks lets a caller observe progress and cancel. Both fields may be nil.
type Hooks struct {
	// Cancel, when closed, makes Compute return ErrCancelled at the next step.
	Cancel <-chan struct{}
	// Progress is called after every integrated step.
	Progress func(doneMetres, totalMetres float64)
}

const (
	// normalMatchTolerance is the 1 cm engineering tolerance used for the
	// "back at normal depth at the upstream end" flag.
	normalMatchTolerance = 0.01
	// criticalSlopeRelTolerance flags yn ~= yc as a critical-slope reach.
	criticalSlopeRelTolerance = 1e-6
	// criticalGuard keeps a tiny subcritical margin above yc when bisection
	// needs a finite lower bracket.
	criticalGuard  = 1e-9
	rootIterations = 200
)

// reachHydraulics caches the quantities constant within a reach.
type reachHydraulics struct {
	index      int // storage index, 0 = most upstream
	r          channel.Reach
	in         manning.Input
	yn, yc     float64
	sDown, sUp float64 // distance of reach ends from the control
}

// Compute integrates the profile for def at its design discharge, starting
// from the downstream control depth.
func Compute(def channel.Definition, hooks Hooks) (*Profile, error) {
	return ComputeFromDepth(def, 0, 0, true, hooks)
}

// ComputePrefix integrates only the reaches whose storage index is in
// [0, firstReach] (i.e. the upstream-most firstReach+1 reaches), starting from
// a known subcritical depth at the downstream end of reach firstReach.
//
// It is used by the partial-recompute path: the unchanged downstream suffix
// is not re-integrated. Distances are still measured from the (unchanged)
// downstream control, so failure locations and point coordinates stay
// globally consistent. The caller is responsible for appending suffix points
// and suffix reach summaries itself.
func ComputePrefix(def channel.Definition, firstReach int, startDepth float64, hooks Hooks) (*Profile, error) {
	return ComputeFromDepth(def, firstReach, startDepth, false, hooks)
}

// ComputeFromDepth is the shared integrator. When useControl is true the
// starting depth comes from the downstream control and every reach is
// integrated; otherwise integration covers reaches 0..firstReach and starts
// from startDepth at firstReach's downstream end.
func ComputeFromDepth(def channel.Definition, firstReach int, startDepth float64, useControl bool, hooks Hooks) (*Profile, error) {
	if err := def.Validate(); err != nil {
		return nil, err
	}
	q := def.DesignFlow

	var controlDepth float64
	if useControl {
		d, err := def.DownstreamDepth()
		if err != nil {
			return nil, err
		}
		controlDepth = d
	} else {
		if firstReach < 0 || firstReach >= len(def.Reaches) {
			return nil, fmt.Errorf("profile: start reach %d out of range", firstReach)
		}
		if !(startDepth > 0) {
			return nil, fmt.Errorf("profile: partial start depth must be positive, got %.6g", startDepth)
		}
		controlDepth = startDepth
	}

	total := def.TotalLength()

	// Locate each reach along the s-axis (distance upstream of the control).
	// Reaches are stored upstream-first; the last reach ends at s = 0.
	rh := make([]reachHydraulics, len(def.Reaches))
	cumUp := total
	for i, r := range def.Reaches {
		in := manning.Input{Section: r.Section, Roughness: r.Roughness, Slope: r.Slope, Flow: q}
		yn, err := manning.NormalDepth(in)
		if err != nil {
			return nil, fmt.Errorf("normal depth for reach %d: %w", i, err)
		}
		yc := flow.CriticalDepth(r.Section, q)
		sUp := cumUp
		sDown := cumUp - r.Length
		cumUp = sDown
		rh[i] = reachHydraulics{index: i, r: r, in: in, yn: yn, yc: yc, sUp: sUp, sDown: sDown}
	}

	// Boundary condition must itself be subcritical in the reach it applies to.
	boundaryReach := firstReach
	if useControl {
		boundaryReach = len(rh) - 1
	}
	br := rh[boundaryReach]
	if !(controlDepth > br.yc) {
		return nil, &ComputeFailure{
			Kind:     FailControlDepth,
			Reach:    br.index,
			Distance: br.sDown,
			Message: fmt.Sprintf("starting depth %.6g m at reach %d is at or below its critical depth %.6g m",
				controlDepth, br.index, br.yc),
		}
	}

	prof := &Profile{
		DesignFlow:      q,
		DownstreamDepth: controlDepth,
		TotalLength:     total,
		StepTarget:      DefaultStep,
	}
	reachResults := make([]ReachResult, len(rh))
	for i := range rh {
		reachResults[i] = ReachResult{
			Index:         rh[i].index,
			NormalDepth:   rh[i].yn,
			CriticalDepth: rh[i].yc,
		}
	}

	// March upstream, reach by reach. Full mode starts at the last reach;
	// partial mode starts at the boundary reach (downstream end) with the
	// supplied depth.
	y := controlDepth
	done := 0.0
	startK := len(rh) - 1
	if !useControl {
		startK = firstReach
		// Suffix length is treated as already completed for progress.
		done = rh[firstReach].sDown
	}
	for k := startK; k >= 0; k-- {
		h := rh[k]

		// Steep / critical reaches have no subcritical normal depth: the
		// requested curve would have to pass through critical flow. Fail
		// before integrating a single step of this reach.
		switch {
		case math.Abs(h.yn-h.yc) <= criticalSlopeRelTolerance*math.Max(h.yc, 1e-12):
			return nil, &ComputeFailure{
				Kind:     FailCriticalSlope,
				Reach:    h.index,
				Distance: h.sDown,
				Message:  fmt.Sprintf("reach %d is at critical slope (yn %.6g ~= yc %.6g m)", h.index, h.yn, h.yc),
			}
		case h.yn < h.yc:
			return nil, &ComputeFailure{
				Kind:     FailSteepReach,
				Reach:    h.index,
				Distance: h.sDown,
				Message:  fmt.Sprintf("reach %d is steep: normal depth %.6g m is below critical depth %.6g m", h.index, h.yn, h.yc),
			}
		}
		if y <= h.yc {
			return nil, &ComputeFailure{
				Kind:     FailCriticalCrossing,
				Reach:    h.index,
				Distance: h.sDown,
				Message: fmt.Sprintf("flow entering reach %d at depth %.6g m is at or below its critical depth %.6g m",
					h.index, y, h.yc),
			}
		}

		n := int(math.Ceil(h.r.Length / DefaultStep))
		if n < 1 {
			n = 1
		}
		dx := h.r.Length / float64(n)

		yStart := y
		addPoint(prof, h, q, h.sDown, y)
		for j := 1; j <= n; j++ {
			if isCancelled(hooks.Cancel) {
				return nil, ErrCancelled
			}
			yu, err := stepUpstream(h, q, y, dx)
			if err != nil {
				if cf, ok := err.(*ComputeFailure); ok && cf.Distance == 0 {
					// First step of this reach lands at sDown+dx.
					cf.Distance = h.sDown + dx
				}
				return nil, err
			}
			y = yu
			s := h.sDown + float64(j)*dx
			addPoint(prof, h, q, s, y)
			done += dx
			if hooks.Progress != nil {
				hooks.Progress(done, total)
			}
		}

		rr := reachResults[h.index]
		rr.DepthDown = yStart
		rr.DepthUp = y
		rr.Curve = classifyCurve(yStart, y, h.yn)
		reachResults[h.index] = rr
	}

	prof.Reaches = reachResults

	// Each interior junction was added once as the upstream end of one reach
	// and once as the downstream end of the next; keep one station there.
	// The later occurrence (marching upstream) is kept, so a junction station
	// carries the label of its UPSTREAM reach, consistently with
	// reachAtDistance in the partial-recompute path.
	if len(prof.Points) > 1 {
		dedup := prof.Points[:1]
		for _, pt := range prof.Points[1:] {
			if math.Abs(pt.Distance-dedup[len(dedup)-1].Distance) <= 1e-9 {
				dedup[len(dedup)-1] = pt
			} else {
				dedup = append(dedup, pt)
			}
		}
		prof.Points = dedup
	}

	top := rh[0]
	topY := prof.Points[len(prof.Points)-1].Depth
	prof.UpstreamDepthError = math.Abs(topY - top.yn)
	prof.UpstreamNormalMatch = prof.UpstreamDepthError <= normalMatchTolerance

	return prof, nil
}

// addPoint appends a station; velocity and Froude number come from the shared
// flow/geometry packages.
func addPoint(p *Profile, h reachHydraulics, q, s, y float64) {
	qq := flow.AtDepth(h.r.Section, y, q)
	p.Points = append(p.Points, Point{
		Distance: s,
		Reach:    h.index,
		Depth:    y,
		Velocity: qq.Velocity,
		Froude:   qq.Froude,
	})
}

// specificEnergy returns E(y) = y + Q^2/(2g A^2).
func specificEnergy(h reachHydraulics, q, y float64) float64 {
	a := h.r.Section.Area(y)
	v := q / a
	return y + v*v/(2*gConst)
}

// frictionSlope reuses the existing Manning relation: at bed slope S0 the
// Manning discharge would be Qm = Discharge(y); carrying Q instead gives
// Sf = S0*(Q/Qm)^2. No second Manning implementation exists here.
func frictionSlope(h reachHydraulics, q, y float64) float64 {
	qm := manning.Discharge(h.in, y)
	if qm == 0 {
		return math.Inf(1)
	}
	r := q / qm
	return h.r.Slope * r * r
}

// stepUpstream solves the implicit trapezoidal energy step for the depth one
// dx upstream of depth yd. It fails rather than return a depth at or below yc.
func stepUpstream(h reachHydraulics, q, yd, dx float64) (float64, error) {
	ed := specificEnergy(h, q, yd)
	sfd := frictionSlope(h, q, yd)

	// Residual: E(yu) - Ed - (0.5*(Sfd+Sf(yu)) - S0)*dx = 0.
	residual := func(yu float64) float64 {
		return specificEnergy(h, q, yu) - ed -
			(0.5*(sfd+frictionSlope(h, q, yu))-h.r.Slope)*dx
	}

	// The root must live on the subcritical limb.
	lo := h.yc * (1 + criticalGuard)
	if rl := residual(lo); !(rl < 0) {
		return 0, &ComputeFailure{
			Kind:     FailCriticalCrossing,
			Reach:    h.index,
			Distance: 0, // filled in by caller (sDown + dx of the failing step)
			Message: fmt.Sprintf("step from depth %.6g m would cross the critical depth %.6g m (Fr would reach 1)",
				yd, h.yc),
		}
	}

	// Bracket from above. Starting at max(yd, 2*lo) covers both M1 (root
	// above yd) and M2 (root between yc and yd, in which case yd already
	// brackets it).
	hi := math.Max(yd, lo*2)
	for range 100 {
		if residual(hi) >= 0 {
			break
		}
		hi *= 2
	}
	if residual(hi) < 0 {
		return 0, fmt.Errorf("profile: internal bracketing failure in reach %d", h.index)
	}

	for range rootIterations {
		mid := (lo + hi) / 2
		if residual(mid) < 0 {
			lo = mid
		} else {
			hi = mid
		}
	}
	yu := (lo + hi) / 2

	// Snap a step numerically indistinguishable from the normal-depth
	// asymptote so floating-point noise can never put a point a microscopic
	// amount on the wrong side of yn.
	if math.Abs(yd-h.yn) < 1e-7 && math.Abs(yu-h.yn) < 1e-9 {
		yu = h.yn
	}
	return yu, nil
}

// classifyCurve labels a reach from the depths at its two ends relative to
// the reach normal depth.
func classifyCurve(yDown, yUp, yn float64) string {
	const tol = 1e-6
	if math.Abs(yDown-yn) <= tol && math.Abs(yUp-yn) <= tol {
		return CurveUniform
	}
	// Backwater (M1): both ends on/above the asymptote. Drawdown (M2): both
	// on/below it. A GVF curve on one mild limb cannot straddle yn; the
	// fallbacks only guard against tolerance edge cases.
	if yDown >= yn-tol && yUp >= yn-tol {
		return CurveBackwater
	}
	if yDown <= yn+tol && yUp <= yn+tol {
		return CurveDrawdown
	}
	if (yDown+yUp)/2 >= yn {
		return CurveBackwater
	}
	return CurveDrawdown
}

func isCancelled(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
