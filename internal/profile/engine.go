package profile

import (
	"context"
	"errors"
	"fmt"
	"math"

	"openchannel/internal/flow"
	"openchannel/internal/manning"
	"openchannel/internal/weir"
)

// Numerical choices (see package doc / README for the rationale):
//
//   - Method: classical 4th-order Runge-Kutta on dy/dxi directly. For these
//     smooth, slowly-varying curves the RK4 truncation error at 20 m steps is
//     far below engineering precision (a pinned h=10 m comparison test keeps
//     the two profiles within ~1e-6 m). Cost: four ODE evaluations per
//     station, i.e. 1200 evaluations for a 6 km reach — negligible.
//   - Base step: 20 m, one station per step, so the reported stations need no
//     interpolation on a 20 m grid; reaches shorter than a step take a
//     single step.
//   - Troubled steps (non-finite results, Froude approaching 1, or a step
//     that jumps across the normal-depth asymptote) are halved repeatedly
//     down to a 5 cm floor. At the asymptote the step is accepted on the
//     normal depth; near critical depth the job fails instead of crossing
//     Fr == 1.
//   - Reach seams: bed elevations are assumed continuous (no local
//     contraction/expansion loss); depth is continuous, and velocity/Fr are
//     simply re-evaluated with the upstream reach's own geometry.
const (
	defaultStep = 20.0
	minStep     = 0.05
	frGuard     = 0.999 // closer than this to critical is treated as a crossing
	// slopeClassTol distinguishes mild from critical/steep.
	slopeClassTol = 1e-6
)

// ErrCanceled is returned by Compute when the hook context is canceled.
var ErrCanceled = errors.New("profile: computation canceled")

// Hooks control execution from the asynchronous job layer.
type Hooks struct {
	Context   context.Context // cancellation; nil means never cancel
	Progress  func(xi, total float64)
	StepDelay func() // optional artificial delay per accepted step (tests)
}

// Options tunes integration; the zero value is fine.
type Options struct {
	Step float64 // base upstream step in metres; <= 0 means defaultStep
}

// reachHyd holds the pre-computed per-reach hydraulic anchors, all taken
// from the existing solvers.
type reachHyd struct {
	spec  *ReachSpec
	yn    float64
	yc    float64
	class string
}

// Compute pushes the profile from the downstream control through every
// reach. A non-nil *Failure means the configuration is hydraulic
// impossible under this service's scope (steep/critical reach or a critical
// crossing), not a bad request.
func Compute(spec Spec, opts Options, hooks Hooks) (*Profile, *Failure, error) {
	if err := spec.Validate(); err != nil {
		return nil, nil, err
	}

	step := opts.Step
	if step <= 0 {
		step = defaultStep
	}

	// ---- downstream control depth (inverted weir relation or fixed depth) ----
	y0 := spec.Control.Depth
	if spec.Control.Type == ControlTypeWeir {
		w := spec.Control.Weir
		h, err := weir.Head(w.Width, w.DischargeCoefficient, spec.Flow)
		if err != nil {
			return nil, nil, err
		}
		y0 = h + w.CrestHeight
	}

	// ---- anchors for every reach: normal depth (manning) and critical
	// depth (flow), both via the existing solvers ----
	hyd := make([]reachHyd, len(spec.Reaches))
	for i := range spec.Reaches {
		r := &spec.Reaches[i]
		in := manning.Input{
			Section:   r.Section,
			Roughness: r.Roughness,
			Slope:     r.Slope,
			Flow:      spec.Flow,
		}
		yn, err := manning.NormalDepth(in)
		if err != nil {
			return nil, nil, fmt.Errorf("reach %d normal depth: %w", i, err)
		}
		yc, err := flow.CriticalDepth(r.Section, spec.Flow)
		if err != nil {
			return nil, nil, fmt.Errorf("reach %d critical depth: %w", i, err)
		}
		class := SlopeMild
		switch {
		case yn < yc*(1.0-slopeClassTol):
			class = SlopeSteep
		case math.Abs(yn-yc) <= slopeClassTol*yc:
			class = SlopeCritical
		}
		hyd[i] = reachHyd{spec: r, yn: yn, yc: yc, class: class}
	}

	// ---- march upstream, reach by reach ----
	results := make([]ReachResult, len(spec.Reaches))
	y := y0
	xi := 0.0
	total := 0.0
	for _, r := range spec.Reaches {
		total += r.Length
	}

	for k := len(spec.Reaches) - 1; k >= 0; k-- {
		h := hyd[k]

		// A subcritical push cannot enter a steep/critical reach without a
		// hydraulic jump, which this service does not compute.
		if h.class == SlopeSteep {
			return nil, &Failure{
				Code:                FailureSteepReach,
				ReachIndex:          k,
				ReachName:           h.spec.Name,
				DistanceFromControl: xi,
				Message: fmt.Sprintf("reach %d (%q) is steep: normal depth %.4f m is below critical depth %.4f m "+
					"at Q = %.4f m3/s; a subcritical profile cannot enter it without a hydraulic jump",
					k, label(h.spec.Name), h.yn, h.yc, spec.Flow),
			}, nil
		}
		if h.class == SlopeCritical {
			return nil, &Failure{
				Code:                FailureCriticalSlope,
				ReachIndex:          k,
				ReachName:           h.spec.Name,
				DistanceFromControl: xi,
				Message: fmt.Sprintf("reach %d (%q) is at critical slope (yn = yc = %.4f m); "+
					"the GVF denominator 1 - Fr^2 vanishes throughout",
					k, label(h.spec.Name), h.yn),
			}, nil
		}

		// The downstream boundary of THIS reach is the first station.
		if y <= h.yc || flow.AtDepth(h.spec.Section, y, spec.Flow).Froude >= frGuard {
			fr := flow.AtDepth(h.spec.Section, y, spec.Flow).Froude
			return nil, &Failure{
				Code:                FailureControlNotSubcritical,
				ReachIndex:          k,
				ReachName:           h.spec.Name,
				DistanceFromControl: xi,
				Message: fmt.Sprintf("depth %.4f m at the downstream boundary of reach %d is at or below its "+
					"critical depth %.4f m (Fr = %.3f)", y, k, h.yc, fr),
			}, nil
		}

		rr := ReachResult{
			Index:         k,
			Name:          h.spec.Name,
			Length:        h.spec.Length,
			NormalDepth:   h.yn,
			CriticalDepth: h.yc,
			SlopeClass:    h.class,
			Points:        []Point{mkPoint(k, h.spec.Name, xi, y, spec.Flow, h.spec, false)},
		}

		remaining := h.spec.Length
		for remaining > 0 {
			if hooks.Context != nil && hooks.Context.Err() != nil {
				return nil, nil, ErrCanceled
			}
			ds := math.Min(step, remaining)
			ny, why := attemptStep(h, spec.Flow, y, ds)
			for why != "" {
				if ds <= minStep {
					if why == stepOvershoot {
						ny = h.yn
						break
					}
					// non-finite or critical: refuse to cross Fr == 1.
					return nil, &Failure{
						Code:                FailureCriticalCrossing,
						ReachIndex:          k,
						ReachName:           h.spec.Name,
						DistanceFromControl: xi + ds,
						Message: fmt.Sprintf("profile reaches the critical depth in reach %d (%q), about %.2f m "+
							"upstream of the control (Fr -> 1); depth cannot cross yc = %.4f m without a hydraulic jump",
							k, label(h.spec.Name), xi+ds, h.yc),
					}, nil
				}
				ds /= 2
				ny, why = attemptStep(h, spec.Flow, y, ds)
			}
			if hooks.Progress != nil {
				hooks.Progress(xi+ds, total)
			}
			if hooks.StepDelay != nil {
				hooks.StepDelay()
			}
			xi += ds
			remaining -= ds
			y = ny
			seam := remaining <= toleranceZero
			rr.Points = append(rr.Points, mkPoint(k, h.spec.Name, xi, y, spec.Flow, h.spec, seam))
			if seam {
				break
			}
		}

		// Classify the curve from its downstream-end depth versus yn.
		switch yEnd := rr.Points[0].Depth; {
		case yEnd > h.yn+NormalDepthTolerance:
			rr.CurveType = CurveBackwater
		case yEnd < h.yn-NormalDepthTolerance:
			rr.CurveType = CurveDrawdown
		default:
			rr.CurveType = CurveUniform
		}
		rr.NormalDepthReachedDistance = normalDepthReachedDistance(rr.Points, h.yn)

		results[k] = rr
	}

	prof := &Profile{
		Flow:         spec.Flow,
		TotalLength:  total,
		ControlDepth: y0,
		Reaches:      results,
	}
	prof.UpstreamEnd = upstreamEnd(prof, hyd)
	prof.BackwaterExtentDistance = backwaterExtent(prof, hyd)
	return prof, nil, nil
}

const (
	toleranceZero = 1e-9
)

// derivative returns dy/dxi, where xi increases UPSTREAM from the control.
// The standard GVF form with downstream coordinate x is
//
//	dy/dx = (S0 - Sf) / (1 - Fr^2)
//
// and since xi = const - x,
//
//	dy/dxi = (Sf - S0) / (1 - Fr^2)
//
// Sf comes from manning.FrictionSlope and Fr from flow.AtDepth.
func derivative(h reachHyd, q, y float64) float64 {
	sf := manning.FrictionSlope(h.spec.Section, h.spec.Roughness, y, q)
	fr := flow.AtDepth(h.spec.Section, y, q).Froude
	return (sf - h.spec.Slope) / (1.0 - fr*fr)
}

// step outcome tags.
const (
	stepCritical  = "critical"  // Froude approaches 1 or depth reaches yc
	stepNonFinite = "nonfinite" // non-finite or non-positive result
	stepOvershoot = "overshoot" // step jumps across the yn asymptote
)

// attemptStep advances one classical RK4 step upstream. It reports an
// outcome tag so the caller can halve the step or fail.
func attemptStep(h reachHyd, q, y, d float64) (float64, string) {
	k1 := derivative(h, q, y)
	k2 := derivative(h, q, y+d*k1/2.0)
	k3 := derivative(h, q, y+d*k2/2.0)
	k4 := derivative(h, q, y+d*k3)
	ny := y + d*(k1+2*k2+2*k3+k4)/6.0

	if math.IsNaN(ny) || math.IsInf(ny, 0) || ny <= 0 {
		return ny, stepNonFinite
	}
	if fr := flow.AtDepth(h.spec.Section, ny, q).Froude; fr >= frGuard || ny <= h.yc {
		return ny, stepCritical
	}
	// Within the numerical band of the normal depth, the equilibrium itself
	// is the right answer (the derivative tends to zero there).
	if math.Abs(ny-h.yn) <= 1e-9 {
		return h.yn, ""
	}
	// A step must not jump from one side of the yn asymptote to the other:
	// that would be a discretisation artefact, not physics.
	if (y > h.yn && ny < h.yn) || (y < h.yn && ny > h.yn) {
		return ny, stepOvershoot
	}
	return ny, ""
}

func mkPoint(k int, name string, xi, y, q float64, r *ReachSpec, seam bool) Point {
	qq := flow.AtDepth(r.Section, y, q)
	return Point{
		DistanceFromControl: xi,
		ReachIndex:          k,
		ReachName:           name,
		Depth:               y,
		Velocity:            qq.Velocity,
		Froude:              qq.Froude,
		AtReachBoundary:     seam,
	}
}

func label(name string) string {
	if name == "" {
		return "unnamed"
	}
	return name
}

// normalDepthReached returns the smallest xi within a reach's station list
// from which every station further upstream is within NormalDepthTolerance
// of yn; nil when the reach never settles.
func normalDepthReachedDistance(pts []Point, yn float64) *float64 {
	if len(pts) == 0 {
		return nil
	}
	// Walk from the upstream end while within the band.
	i := len(pts) - 1
	for i >= 0 && math.Abs(pts[i].Depth-yn) <= NormalDepthTolerance {
		i--
	}
	if i == len(pts)-1 {
		return nil // upstream end itself outside the band
	}
	v := pts[i+1].DistanceFromControl
	return &v
}

func upstreamEnd(p *Profile, hyd []reachHyd) UpstreamEnd {
	head := p.Reaches[0].Points[len(p.Reaches[0].Points)-1]
	yn := hyd[0].yn
	diff := math.Abs(head.Depth - yn)
	return UpstreamEnd{
		DistanceFromControl: head.DistanceFromControl,
		Depth:               head.Depth,
		NormalDepth:         yn,
		Difference:          diff,
		AtNormalDepth:       diff <= NormalDepthTolerance,
	}
}

// backwaterExtent walks from the channel head TOWARDS the control while the
// depth stays within 1 cm of the local reach normal depth; the first
// departure fixes the extent. Returns nil when even the channel head is
// outside the band.
func backwaterExtent(p *Profile, hyd []reachHyd) *float64 {
	flat := p.depthNodes()
	ynAt := func(pt Point) float64 { return hyd[pt.ReachIndex].yn }

	if math.Abs(flat[len(flat)-1].Depth-ynAt(flat[len(flat)-1])) > NormalDepthTolerance {
		return nil
	}
	// flat runs downstream -> upstream; iterate from the head backwards and
	// stop at the first station outside the band.
	for i := len(flat) - 2; i >= 0; i-- {
		if math.Abs(flat[i].Depth-ynAt(flat[i])) > NormalDepthTolerance {
			v := flat[i+1].DistanceFromControl
			return &v
		}
	}
	// Whole line within the band: no meaningful perturbation.
	v := 0.0
	return &v
}
