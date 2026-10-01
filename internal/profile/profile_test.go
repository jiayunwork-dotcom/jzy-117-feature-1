package profile

import (
	"errors"
	"math"
	"testing"

	"openchannel/internal/channel"
	"openchannel/internal/geometry"
)

// caseIDefinition is the pinned example: single rectangular reach b = 3 m,
// n = 0.015, S0 = 0.001, Q = 5 m^3/s, L = 6 km, controlled by a 3 m wide
// sharp-crested weir with crest height 1.0 m and Cd = 0.62.
func caseIDefinition() channel.Definition {
	return channel.Definition{
		Reaches: []channel.Reach{{
			Length:    6000,
			Section:   geometry.Section{BottomWidth: 3},
			Roughness: 0.015,
			Slope:     0.001,
		}},
		DesignFlow: 5,
		Control: channel.Control{
			Kind:        channel.ControlWeir,
			Width:       3,
			CrestHeight: 1.0,
			Cd:          0.62,
		},
	}
}

// TestPinnedBackwaterProfile nails the friend's reference numbers (case I):
// depth at the weir ~1.939 m, 1.276 m 1 km upstream, 1.090 m 2 km upstream,
// within 1 cm of the 1.079 m normal depth beyond 3.5 km; the profile must
// fall monotonically upstream and never dip below normal depth.
func TestPinnedBackwaterProfile(t *testing.T) {
	p, err := Compute(caseIDefinition(), Hooks{})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}

	refs := []struct {
		distance float64
		want     float64
	}{
		{0, 1.939},
		{1000, 1.276},
		{2000, 1.090},
	}
	for _, ref := range refs {
		y := DepthAt(p, ref.distance)
		if math.Abs(y-ref.want) > 0.01 {
			t.Errorf("depth at s=%.0f m = %.4f, want %.3f +/- 0.01 m",
				ref.distance, y, ref.want)
		}
	}

	yn := p.Reaches[0].NormalDepth
	if math.Abs(yn-1.079) > 0.001 {
		t.Errorf("normal depth = %.4f, want ~1.079 m", yn)
	}

	// Beyond 3.5 km the profile is within 1 cm of the normal depth.
	for _, s := range []float64{3500, 4000, 5000, 6000} {
		if d := math.Abs(DepthAt(p, s) - yn); d > 0.01 {
			t.Errorf("|y(%.0f)-yn| = %.4f m, want <= 0.01 m", s, d)
		}
	}

	// Monotone non-increasing upstream and never below normal depth.
	prev := math.Inf(1)
	for _, pt := range p.Points {
		if pt.Depth > prev+1e-9 {
			t.Fatalf("depth increased upstream at s=%.1f (%.6f > %.6f)",
				pt.Distance, pt.Depth, prev)
		}
		if pt.Depth < yn-1e-9 {
			t.Fatalf("depth %.6f below normal depth %.6f at s=%.1f", pt.Depth, yn, pt.Distance)
		}
		prev = pt.Depth
	}

	if !p.UpstreamNormalMatch {
		t.Errorf("upstream end error %.2e m should be within 1 cm of yn", p.UpstreamDepthError)
	}
	if p.Reaches[0].Curve != CurveBackwater {
		t.Errorf("curve = %q, want backwater", p.Reaches[0].Curve)
	}
	if math.Abs(p.DownstreamDepth-1.939) > 0.001 {
		t.Errorf("downstream depth = %.4f, want ~1.939 m", p.DownstreamDepth)
	}
}

// TestDownstreamDepthEqualsNormalDepthIsUniform is case II: a prescribed
// downstream depth exactly equal to the normal depth must produce uniform
// flow along the whole reach.
func TestDownstreamDepthEqualsNormalDepthIsUniform(t *testing.T) {
	def := caseIDefinition()
	yn, err := normalDepthOf(def, 0)
	if err != nil {
		t.Fatal(err)
	}
	def.Control = channel.Control{Kind: channel.ControlDepth, Depth: yn}

	p, err := Compute(def, Hooks{})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	for _, pt := range p.Points {
		if math.Abs(pt.Depth-yn) > 1e-8 {
			t.Fatalf("depth %.9f at s=%.0f deviates from yn %.9f", pt.Depth, pt.Distance, yn)
		}
	}
	if p.Reaches[0].Curve != CurveUniform {
		t.Errorf("curve = %q, want uniform", p.Reaches[0].Curve)
	}
	if !p.UpstreamNormalMatch || p.UpstreamDepthError > 1e-10 {
		t.Errorf("upstream end error = %.2e", p.UpstreamDepthError)
	}
}

// TestM2DrawdownApproachesNormalDepth is case III: on a mild reach, a
// prescribed depth between yc and yn produces an M2 drawdown curve that rises
// upstream toward yn and never crosses it.
func TestM2DrawdownApproachesNormalDepth(t *testing.T) {
	def := channel.Definition{
		Reaches: []channel.Reach{{
			Length:    8000,
			Section:   geometry.Section{BottomWidth: 3},
			Roughness: 0.015,
			Slope:     0.001,
		}},
		DesignFlow: 5,
	}
	yn, err := normalDepthOf(def, 0)
	if err != nil {
		t.Fatal(err)
	}
	yc := criticalDepthOf(def, 0)
	if !(yn > yc) {
		t.Fatalf("test setup not mild: yn %.4f yc %.4f", yn, yc)
	}

	def.Control = channel.Control{
		Kind:  channel.ControlDepth,
		Depth: yc + 0.5*(yn-yc), // strictly between yc and yn
	}
	p, err := Compute(def, Hooks{})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}

	if p.Reaches[0].Curve != CurveDrawdown {
		t.Errorf("curve = %q, want drawdown", p.Reaches[0].Curve)
	}
	prev := yc // depths must rise upstream ...
	for _, pt := range p.Points {
		if pt.Depth < prev-1e-9 {
			t.Fatalf("depth did not rise monotonically upstream at s=%.0f", pt.Distance)
		}
		if pt.Depth > yn+1e-9 {
			t.Fatalf("depth %.6f crossed above normal depth %.6f at s=%.0f", pt.Depth, yn, pt.Distance)
		}
		prev = pt.Depth
	}
	if !p.UpstreamNormalMatch {
		t.Errorf("upstream end error %.2e m should be within 1 cm of yn", p.UpstreamDepthError)
	}
}

// TestRaisingWeirCrestRaisesEveryDepth is case IV at the engine level:
// increasing only the crest height must not lower the water depth anywhere.
func TestRaisingWeirCrestRaisesEveryDepth(t *testing.T) {
	low := caseIDefinition()
	high := caseIDefinition()
	high.Control.CrestHeight = 1.2

	pLow, err := Compute(low, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	pHigh, err := Compute(high, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	// Sample the lower-crest profile at every one of its stations.
	for _, pt := range pLow.Points {
		yHigh := DepthAt(pHigh, pt.Distance)
		if yHigh < pt.Depth-1e-9 {
			t.Fatalf("crest raise lowered depth at s=%.0f: %.6f -> %.6f",
				pt.Distance, pt.Depth, yHigh)
		}
	}
}

// TestSteepReachFails is case VI (steep reach): the job fails with the steep
// reach identified, before any curve is returned.
func TestSteepReachFails(t *testing.T) {
	mild := channel.Reach{
		Length:    2000,
		Section:   geometry.Section{BottomWidth: 3},
		Roughness: 0.015,
		Slope:     0.001,
	}
	// Steep: S0 large enough that yn < yc for Q = 5.
	steep := channel.Reach{
		Length:    2000,
		Section:   geometry.Section{BottomWidth: 3},
		Roughness: 0.015,
		Slope:     0.05,
	}
	ynSteep, err := normalDepthOf(channel.Definition{Reaches: []channel.Reach{steep}, DesignFlow: 5}, 0)
	if err != nil {
		t.Fatal(err)
	}
	ycSteep := criticalDepthOf(channel.Definition{Reaches: []channel.Reach{steep}, DesignFlow: 5}, 0)
	if !(ynSteep < ycSteep) {
		t.Fatalf("test setup: yn %.4f not below yc %.4f", ynSteep, ycSteep)
	}

	def := channel.Definition{
		// upstream steep, downstream mild
		Reaches:    []channel.Reach{steep, mild},
		DesignFlow: 5,
		Control:    channel.Control{Kind: channel.ControlDepth, Depth: 1.5},
	}
	_, err = Compute(def, Hooks{})
	var cf *ComputeFailure
	if !errors.As(err, &cf) {
		t.Fatalf("want ComputeFailure, got %v", err)
	}
	if cf.Kind != FailSteepReach {
		t.Errorf("kind = %q, want %q", cf.Kind, FailSteepReach)
	}
	if cf.Reach != 0 {
		t.Errorf("reach = %d, want 0", cf.Reach)
	}
	// Steep reach begins 2000 m upstream of the control.
	if math.Abs(cf.Distance-2000) > 1e-6 {
		t.Errorf("distance = %.1f, want 2000 m", cf.Distance)
	}
}

// TestCriticalCrossingAtJunctionFails is case VI (crossing critical depth):
// a narrow upstream reach whose yc lies above the depth transmitted by the
// wide downstream reach must fail at their junction, naming the place.
func TestCriticalCrossingAtJunctionFails(t *testing.T) {
	wide := channel.Reach{
		Length:    3000,
		Section:   geometry.Section{BottomWidth: 8},
		Roughness: 0.015,
		Slope:     0.001,
	}
	narrow := channel.Reach{
		Length:    1000,
		Section:   geometry.Section{BottomWidth: 1.0},
		Roughness: 0.015,
		Slope:     0.001,
	}
	def := channel.Definition{
		Reaches:    []channel.Reach{narrow, wide},
		DesignFlow: 5,
		// Depth below the narrow section's yc but above the wide one's.
		Control: channel.Control{Kind: channel.ControlDepth, Depth: 1.2},
	}
	ycNarrow := criticalDepthOf(def, 0)
	ycWide := criticalDepthOf(def, 1)
	if !(1.2 < ycNarrow && 1.2 > ycWide) {
		t.Fatalf("test setup: yc narrow %.3f, yc wide %.3f, boundary 1.2", ycNarrow, ycWide)
	}
	_, err := Compute(def, Hooks{})
	var cf *ComputeFailure
	if !errors.As(err, &cf) {
		t.Fatalf("want ComputeFailure, got %v", err)
	}
	if cf.Kind != FailCriticalCrossing {
		t.Errorf("kind = %q, want %q", cf.Kind, FailCriticalCrossing)
	}
	if cf.Reach != 0 {
		t.Errorf("reach = %d, want 0", cf.Reach)
	}
	if math.Abs(cf.Distance-3000) > 1e-6 {
		t.Errorf("distance = %.1f, want 3000 m (the junction)", cf.Distance)
	}
}

// TestControlAtOrBelowCriticalFails rejects a non-subcritical boundary.
func TestControlAtOrBelowCriticalFails(t *testing.T) {
	def := caseIDefinition()
	yc := criticalDepthOf(def, 0)
	def.Control = channel.Control{Kind: channel.ControlDepth, Depth: yc}
	if _, err := Compute(def, Hooks{}); err == nil {
		t.Fatal("expected failure for control depth == yc")
	}
}

// TestJunctionContinuity checks depth continuity where two reaches meet: the
// shared station appears once and the depths from both sides are identical.
func TestJunctionContinuity(t *testing.T) {
	r1 := channel.Reach{
		Length:    2500, // not a multiple of the 50 m target in an awkward way
		Section:   geometry.Section{BottomWidth: 3, SideSlope: 0.5},
		Roughness: 0.02,
		Slope:     0.0008,
	}
	r2 := channel.Reach{
		Length:    1700,
		Section:   geometry.Section{BottomWidth: 3},
		Roughness: 0.015,
		Slope:     0.0012,
	}
	def := channel.Definition{
		Reaches:    []channel.Reach{r1, r2},
		DesignFlow: 4,
		Control:    channel.Control{Kind: channel.ControlDepth, Depth: 1.6},
	}
	p, err := Compute(def, Hooks{})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	// Exactly one point at the junction distance (1700 m), no duplicated
	// station with two different depths.
	var atJunction []Point
	for _, pt := range p.Points {
		if math.Abs(pt.Distance-1700) < 1e-9 {
			atJunction = append(atJunction, pt)
		}
	}
	if len(atJunction) != 1 {
		t.Fatalf("junction station count = %d, want 1", len(atJunction))
	}
	// Grid endpoints exactly: 0 and total length.
	if math.Abs(p.Points[0].Distance) > 1e-9 {
		t.Errorf("first station s = %v, want 0", p.Points[0].Distance)
	}
	last := p.Points[len(p.Points)-1]
	if math.Abs(last.Distance-4200) > 1e-9 {
		t.Errorf("last station s = %v, want 4200", last.Distance)
	}
	if last.Reach != 0 {
		t.Errorf("last station reach = %d, want 0", last.Reach)
	}
}

// TestCancellation verifies the engine honours a closed cancel channel and
// never returns a profile.
func TestCancellation(t *testing.T) {
	cancel := make(chan struct{})
	hooks := Hooks{
		Cancel: cancel,
		Progress: func(done, total float64) {
			if done >= 1000 {
				close(cancel)
			}
		},
	}
	_, err := Compute(caseIDefinition(), hooks)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled", err)
	}
}
