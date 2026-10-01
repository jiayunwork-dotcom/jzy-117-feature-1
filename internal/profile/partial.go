package profile

import (
	"fmt"
	"math"

	"openchannel/internal/channel"
)

// Recompute modes exposed to callers.
const (
	ModeFull    = "full"
	ModePartial = "partial"
)

// PartialEligible reports whether a profile for newDef can reuse the
// downstream tail of the profile computed for oldDef.
//
// Subcritical GVF is marched upstream, so a change can only affect stations
// upstream of the most downstream changed reach. The suffix is reusable when
// all of these hold:
//
//  1. the design discharge is unchanged;
//  2. the downstream control is identical;
//  3. a non-empty TAIL of reach specifications (section, n, S0, length) is
//     identical between the two definitions, so the same grid points there
//     carry the same depths. This holds whether reaches upstream of the tail
//     were changed, inserted or removed (including the whole old upstream
//     prefix disappearing).
//
// Everything else (changed discharge/control, changed tail reach) falls back
// to a full recompute. The first return value is the storage index of the
// first reach that needs integrating.
func PartialEligible(oldDef, newDef channel.Definition) (firstReach int, eligible bool) {
	if oldDef.DesignFlow == newDef.DesignFlow &&
		controlsEqual(oldDef.Control, newDef.Control) {
		if k := commonSuffixLength(oldDef.Reaches, newDef.Reaches); k > 0 {
			return len(newDef.Reaches) - k, true
		}
	}
	return len(newDef.Reaches) - 1, false
}

func commonSuffixLength(a, b []channel.Reach) int {
	k := 0
	for k < len(a) && k < len(b) && reachesEqual(a[len(a)-1-k], b[len(b)-1-k]) {
		k++
	}
	return k
}

func reachesEqual(x, y channel.Reach) bool {
	return x.Length == y.Length &&
		x.Roughness == y.Roughness &&
		x.Slope == y.Slope &&
		x.Section.BottomWidth == y.Section.BottomWidth &&
		x.Section.SideSlope == y.Section.SideSlope
}

func controlsEqual(a, b channel.Control) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case channel.ControlDepth:
		return a.Depth == b.Depth
	case channel.ControlWeir:
		return a.Width == b.Width && a.CrestHeight == b.CrestHeight && a.Cd == b.Cd
	}
	return false
}

// RecomputePartial performs the partial recompute. It must only be called
// when PartialEligible reports eligible with this pair. The returned profile
// covers the whole new line: recomputed upstream prefix plus unchanged
// downstream points copied verbatim (depths untouched) from oldProf.
//
// Reach indices of the copied suffix are relabelled for the new definition,
// since reaches may have been inserted upstream and storage indices shifted.
func RecomputePartial(oldDef, newDef channel.Definition, firstReach int, oldProf *Profile, hooks Hooks) (*Profile, error) {
	boundaryS := newDef.ReachDownDistance(firstReach)
	prefix, err := ComputePrefix(newDef, firstReach, DepthAt(oldProf, boundaryS), hooks)
	if err != nil {
		return nil, err
	}

	out := &Profile{
		DesignFlow:      newDef.DesignFlow,
		DownstreamDepth: oldProf.DownstreamDepth,
		TotalLength:     newDef.TotalLength(),
		StepTarget:      DefaultStep,
	}
	// Points must stay ordered by ascending s (control -> upstream). Copy the
	// unchanged suffix (s < boundary) first, then the recomputed prefix.
	for _, pt := range oldProf.Points {
		if pt.Distance < boundaryS-1e-9 {
			pt.Reach = reachAtDistance(newDef, pt.Distance)
			out.Points = append(out.Points, pt)
		}
	}
	out.Points = append(out.Points, prefix.Points...)

	out.Reaches = make([]ReachResult, len(newDef.Reaches))
	for i := 0; i <= firstReach; i++ {
		out.Reaches[i] = prefix.Reaches[i]
	}
	// Map suffix summaries by physical location rather than storage index.
	oldByDown := make(map[float64]ReachResult, len(oldProf.Reaches))
	for _, rr := range oldProf.Reaches {
		oldByDown[oldDef.ReachDownDistance(rr.Index)] = rr
	}
	for i := firstReach + 1; i < len(newDef.Reaches); i++ {
		rr, ok := oldByDown[newDef.ReachDownDistance(i)]
		if !ok {
			return nil, fmt.Errorf("profile: partial recompute could not map suffix reach %d", i)
		}
		rr.Index = i
		out.Reaches[i] = rr
	}

	topY := out.Points[len(out.Points)-1].Depth
	yn := out.Reaches[0].NormalDepth
	out.UpstreamDepthError = math.Abs(topY - yn)
	out.UpstreamNormalMatch = out.UpstreamDepthError <= normalMatchTolerance
	return out, nil
}

// reachAtDistance returns the storage index of the reach containing a station
// at distance s upstream of the control. A station exactly at a reach
// junction is assigned to the UPSTREAM reach, matching the engine's
// convention (the shared station keeps the label of the reach whose
// upstream end it is).
func reachAtDistance(def channel.Definition, s float64) int {
	if s <= 1e-9 {
		return len(def.Reaches) - 1
	}
	if s >= def.TotalLength()-1e-9 {
		return 0
	}
	for i := range def.Reaches {
		lo, hi := def.ReachDownDistance(i), def.ReachUpDistance(i)
		if s >= lo-1e-9 && s < hi-1e-9 {
			return i
		}
	}
	return 0
}

// DepthAt linearly interpolates the profile depth at distance s (metres
// upstream of the control) and clips to the profile ends outside its range.
// Points are stored from the control upstream, i.e. increasing s.
func DepthAt(p *Profile, s float64) float64 {
	pts := p.Points
	if len(pts) == 0 {
		return 0
	}
	if s <= pts[0].Distance {
		return pts[0].Depth
	}
	last := pts[len(pts)-1]
	if s >= last.Distance {
		return last.Depth
	}
	for i := 1; i < len(pts); i++ {
		if s <= pts[i].Distance {
			a, b := pts[i-1], pts[i]
			f := (s - a.Distance) / (b.Distance - a.Distance)
			return a.Depth + f*(b.Depth-a.Depth)
		}
	}
	return last.Depth
}
