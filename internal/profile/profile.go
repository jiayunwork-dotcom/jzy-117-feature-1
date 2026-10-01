// Package profile computes steady, gradually varied flow (GVF) water-surface
// profiles along an ordered chain of channel reaches with a downstream
// control (sharp-crested weir or fixed depth).
//
// It contains no hydraulic formulas of its own:
//
//   - section geometry (A, P, T, R) comes from internal/geometry,
//   - the normal depth and the friction slope Sf come from internal/manning
//     (Sf is Manning's equation solved for the slope, one and the same
//     relation),
//   - velocity and the Froude number come from internal/flow,
//   - the weir head that fixes the downstream level comes from internal/weir
//     (the exact inverse of the existing weir endpoint).
//
// Integration direction is UPSTREAM from the control: subcritical flow is
// controlled from downstream, so the profile is well posed as a march in
// that direction. Hydraulic jumps are out of scope; a steep/critical reach
// or any attempt to cross the critical depth ends the job with an explicit
// failure instead of producing a curve through Fr == 1.
package profile

import (
	"encoding/json"
	"math"

	"openchannel/internal/geometry"
	"openchannel/internal/validation"
)

// Distance along the profile, xi, is measured upstream from the downstream
// control section: xi = 0 at the weir/control, increasing towards the
// channel head.

// ReachSpec describes one prismatic reach of the chain. Reaches are given in
// upstream-to-downstream order; the last reach meets the control.
type ReachSpec struct {
	Name      string           `json:"name,omitempty"`
	Length    float64          `json:"length"`    // metres, > 0
	Section   geometry.Section `json:"section"`   // bottom width and side slope
	Roughness float64          `json:"roughness"` // Manning's n (SI), > 0
	Slope     float64          `json:"slope"`     // bed slope S0, > 0
}

// WeirControl is a rectangular sharp-crested weir at the downstream end.
type WeirControl struct {
	Width                float64 `json:"width"`                 // crest width b, m, > 0
	CrestHeight          float64 `json:"crest_height"`          // crest above local channel invert p, m, >= 0
	DischargeCoefficient float64 `json:"discharge_coefficient"` // Cd, > 0
}

// Control is the downstream boundary: either a weir or a fixed water depth.
type Control struct {
	Type  string       `json:"type"` // "weir" or "depth"
	Weir  *WeirControl `json:"weir,omitempty"`
	Depth float64      `json:"depth,omitempty"` // channel water depth at xi = 0, m
}

// Spec is a full channel-line definition.
type Spec struct {
	Reaches []ReachSpec `json:"reaches"` // upstream -> downstream, at least one
	Flow    float64     `json:"flow"`    // design discharge Q, m^3/s, > 0 for a profile
	Control Control     `json:"control"`
}

// Validate checks structural ranges before any hydraulic work. Hydraulic
// feasibility (steep slopes, critical-depth crossings) is NOT a validation
// error — it is a profile failure carrying the offending location.
func (s Spec) Validate() error {
	if len(s.Reaches) == 0 {
		return &validation.Error{Field: "reaches", Message: "at least one reach is required"}
	}
	for i := range s.Reaches {
		r := &s.Reaches[i]
		pfx := "reaches[" + itoa(i) + "]"
		if err := validation.Positive(pfx+".length", r.Length); err != nil {
			return err
		}
		if err := validation.Positive(pfx+".bottom_width", r.Section.BottomWidth); err != nil {
			return err
		}
		if err := validation.NonNegative(pfx+".side_slope", r.Section.SideSlope); err != nil {
			return err
		}
		if err := validation.Positive(pfx+".roughness", r.Roughness); err != nil {
			return err
		}
		if err := validation.Positive(pfx+".slope", r.Slope); err != nil {
			return err
		}
	}
	if err := validation.Positive("flow", s.Flow); err != nil {
		return err
	}
	switch s.Control.Type {
	case ControlTypeWeir:
		if s.Control.Weir == nil {
			return &validation.Error{Field: "control.weir", Message: "weir control requires weir parameters"}
		}
		w := s.Control.Weir
		if err := validation.Positive("control.weir.width", w.Width); err != nil {
			return err
		}
		if err := validation.NonNegative("control.weir.crest_height", w.CrestHeight); err != nil {
			return err
		}
		if err := validation.Positive("control.weir.discharge_coefficient", w.DischargeCoefficient); err != nil {
			return err
		}
	case ControlTypeDepth:
		if err := validation.Positive("control.depth", s.Control.Depth); err != nil {
			return err
		}
	default:
		return &validation.Error{Field: "control.type", Message: `must be "weir" or "depth"`}
	}
	return nil
}

// Control type tags.
const (
	ControlTypeWeir  = "weir"
	ControlTypeDepth = "depth"
)

// reachJSON is the wire form of ReachSpec. The geometry section lives in
// internal/geometry without JSON tags, so the snake_case mapping is done
// here to keep the API consistent.
type reachJSON struct {
	Name    string  `json:"name,omitempty"`
	Length  float64 `json:"length"`
	Section struct {
		BottomWidth float64 `json:"bottom_width"`
		SideSlope   float64 `json:"side_slope"`
	} `json:"section"`
	Roughness float64 `json:"roughness"`
	Slope     float64 `json:"slope"`
}

// MarshalJSON maps the section fields to snake_case on the wire.
func (r ReachSpec) MarshalJSON() ([]byte, error) {
	var v reachJSON
	v.Name = r.Name
	v.Length = r.Length
	v.Section.BottomWidth = r.Section.BottomWidth
	v.Section.SideSlope = r.Section.SideSlope
	v.Roughness = r.Roughness
	v.Slope = r.Slope
	return json.Marshal(v)
}

// UnmarshalJSON accepts the snake_case wire form.
func (r *ReachSpec) UnmarshalJSON(b []byte) error {
	var v reachJSON
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	r.Name = v.Name
	r.Length = v.Length
	r.Section = geometry.Section{BottomWidth: v.Section.BottomWidth, SideSlope: v.Section.SideSlope}
	r.Roughness = v.Roughness
	r.Slope = v.Slope
	return nil
}

// Slope classes, from yn versus yc at the design flow.
const (
	SlopeMild     = "mild"     // yn > yc
	SlopeSteep    = "steep"    // yn < yc
	SlopeCritical = "critical" // yn == yc (within numerical tolerance)
)

// Curve labels for a reach under a subcritical push.
const (
	CurveBackwater = "backwater" // depth above normal depth (M1-style), e.g. weir ponding
	CurveDrawdown  = "drawdown"  // depth between yc and yn (M2-style), falling downstream
	CurveUniform   = "uniform"   // depth at the normal depth within tolerance
)

// NormalDepthTolerance is the 1 cm band used to decide whether depth has
// returned to the normal depth, as specified for the channel head.
const NormalDepthTolerance = 0.01

// Point is one computed station on the profile.
type Point struct {
	DistanceFromControl float64 `json:"distance_from_control_m"` // xi, metres upstream of the control
	ReachIndex          int     `json:"reach_index"`             // 0-based, upstream reach first
	ReachName           string  `json:"reach_name,omitempty"`
	Depth               float64 `json:"depth_m"`
	Velocity            float64 `json:"velocity_mps"`
	Froude              float64 `json:"froude_number"`
	// AtReachBoundary marks a reach seam/end. At a seam the point appears
	// once; depth is continuous there by construction, velocity/Fr change
	// with the new section and are reported for the reach the point is
	// assigned to.
	AtReachBoundary bool `json:"at_reach_boundary"`
}

// ReachResult summarises one integrated reach.
type ReachResult struct {
	Index  int     `json:"index"`
	Name   string  `json:"name,omitempty"`
	Length float64 `json:"length_m"`

	NormalDepth   float64 `json:"normal_depth_m"`
	CriticalDepth float64 `json:"critical_depth_m"`
	SlopeClass    string  `json:"slope_class"`
	CurveType     string  `json:"curve_type"`

	// Distance from the control (xi) at which the profile first settles
	// within NormalDepthTolerance of this reach's normal depth and stays
	// there further upstream. Null when the normal depth is not reached
	// inside this reach.
	NormalDepthReachedDistance *float64 `json:"normal_depth_reached_distance_m,omitempty"`

	Points []Point `json:"points"` // downstream -> upstream order
}

// UpstreamEnd describes the most upstream station of the whole line.
type UpstreamEnd struct {
	DistanceFromControl float64 `json:"distance_from_control_m"`
	Depth               float64 `json:"depth_m"`
	NormalDepth         float64 `json:"normal_depth_m"`
	Difference          float64 `json:"difference_m"`
	AtNormalDepth       bool    `json:"at_normal_depth"`
}

// FailureCode enumerates the explicit terminal failures.
type FailureCode string

const (
	// FailureSteepReach: integration entered a reach with yn < yc. A
	// subcritical push cannot continue through it without a hydraulic jump,
	// which is out of scope.
	FailureSteepReach FailureCode = "steep_reach"
	// FailureCriticalSlope: yn == yc reach; the GVF denominator vanishes
	// along the whole reach.
	FailureCriticalSlope FailureCode = "critical_slope_reach"
	// FailureCriticalCrossing: the computed profile reached the critical
	// depth (Fr -> 1) mid-reach.
	FailureCriticalCrossing FailureCode = "critical_depth_crossing"
	// FailureControlNotSubcritical: the downstream boundary depth is at or
	// below the last reach's critical depth.
	FailureControlNotSubcritical FailureCode = "control_depth_not_subcritical"
)

// Failure locates a rejected push.
type Failure struct {
	Code                FailureCode `json:"code"`
	ReachIndex          int         `json:"reach_index"`
	ReachName           string      `json:"reach_name,omitempty"`
	DistanceFromControl float64     `json:"distance_from_control_m"`
	Message             string      `json:"message"`
}

// Profile is a successful result.
type Profile struct {
	Flow         float64 `json:"flow_m3s"`
	TotalLength  float64 `json:"total_length_m"`
	ControlDepth float64 `json:"control_depth_m"` // channel water depth at xi = 0

	Reaches     []ReachResult `json:"reaches"` // upstream -> downstream order
	UpstreamEnd UpstreamEnd   `json:"upstream_end"`

	// BackwaterExtentDistance is the furthest-upstream xi at which the depth
	// still departs from the LOCAL reach normal depth by more than
	// NormalDepthTolerance; beyond it the whole profile has returned to
	// normal depth. Null when the perturbation still exceeds 1 cm at the
	// channel head; 0 when there is no perturbation at all.
	BackwaterExtentDistance *float64 `json:"backwater_extent_distance_m"`
}

// DepthAt returns the depth at any xi by linear interpolation between
// computed stations (depths are continuous across seams). xi is clamped to
// the computed range.
func (p *Profile) DepthAt(xi float64) float64 {
	return sample(p.depthNodes(), xi)
}

func (p *Profile) depthNodes() []Point {
	// One flat downstream->upstream list of all stations.
	n := 0
	for i := range p.Reaches {
		n += len(p.Reaches[i].Points)
	}
	out := make([]Point, 0, n)
	// Reaches are stored upstream->downstream; walk from the last reach.
	for i := len(p.Reaches) - 1; i >= 0; i-- {
		pts := p.Reaches[i].Points
		// Reach points are stored downstream->upstream.
		if len(out) > 0 && len(pts) > 0 {
			first := pts[0]
			if first.DistanceFromControl == out[len(out)-1].DistanceFromControl {
				pts = pts[1:] // drop duplicated seam station
			}
		}
		out = append(out, pts...)
	}
	return out
}

func sample(nodes []Point, xi float64) float64 {
	if len(nodes) == 0 {
		return math.NaN()
	}
	if xi <= nodes[0].DistanceFromControl {
		return nodes[0].Depth
	}
	last := nodes[len(nodes)-1].DistanceFromControl
	if xi >= last {
		return nodes[len(nodes)-1].Depth
	}
	// Nodes are ascending in xi.
	lo, hi := 0, len(nodes)-1
	for hi-lo > 1 {
		mid := (lo + hi) / 2
		if nodes[mid].DistanceFromControl <= xi {
			lo = mid
		} else {
			hi = mid
		}
	}
	x0, x1 := nodes[lo].DistanceFromControl, nodes[hi].DistanceFromControl
	if x1 == x0 {
		return nodes[lo].Depth
	}
	t := (xi - x0) / (x1 - x0)
	return nodes[lo].Depth + t*(nodes[hi].Depth-nodes[lo].Depth)
}

// itoa is a tiny allocation-free integer formatter used for field paths.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
