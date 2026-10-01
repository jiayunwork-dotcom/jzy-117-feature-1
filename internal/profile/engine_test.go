package profile

import (
	"math"
	"testing"

	"openchannel/internal/flow"
	"openchannel/internal/geometry"
	"openchannel/internal/manning"
)

// The friend's headline case, pinned to regression.
// Rectangular b=3, n=0.015, S0=0.001, Q=5, L=6 km; downstream thin-plate
// weir b=3, crest p=1.0, Cd=0.62.
func friendSpec() Spec {
	return Spec{
		Reaches: []ReachSpec{{
			Name:      "R1",
			Length:    6000,
			Section:   geometry.Section{BottomWidth: 3},
			Roughness: 0.015,
			Slope:     0.001,
		}},
		Flow: 5,
		Control: Control{
			Type: ControlTypeWeir,
			Weir: &WeirControl{Width: 3, CrestHeight: 1.0, DischargeCoefficient: 0.62},
		},
	}
}

func TestPinnedBackwaterCase(t *testing.T) {
	p, fail, err := Compute(friendSpec(), Options{}, Hooks{})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if fail != nil {
		t.Fatalf("unexpected failure: %+v", fail)
	}

	want := map[float64]float64{
		0:    1.939, // control (weir ponding)
		1000: 1.276,
		2000: 1.090,
	}
	const tol = 0.01
	for xi, wd := range want {
		got := p.DepthAt(xi)
		if math.Abs(got-wd) > tol {
			t.Errorf("depth at xi=%v = %.4f, want %.4f (±%.2f)", xi, got, wd, tol)
		}
	}

	// Beyond 3.5 km within 1 cm of the normal depth.
	yn := p.Reaches[0].NormalDepth
	if math.Abs(yn-1.079) > 0.005 {
		t.Errorf("normal depth = %.4f, want ~1.079", yn)
	}
	for _, xi := range []float64{3500, 4000, 5000, 6000} {
		got := p.DepthAt(xi)
		if math.Abs(got-yn) > 0.01 {
			t.Errorf("depth at xi=%v = %.4f, within 1 cm of yn=%.4f expected", xi, got, yn)
		}
	}

	// Monotone non-increasing upstream and never below yn.
	nodes := p.depthNodes()
	for i := 1; i < len(nodes); i++ {
		if nodes[i].Depth > nodes[i-1].Depth+1e-9 {
			t.Fatalf("depth increases upstream at xi=%v: %.6f -> %.6f",
				nodes[i].DistanceFromControl, nodes[i-1].Depth, nodes[i].Depth)
		}
		if nodes[i].Depth < yn-1e-9 {
			t.Fatalf("depth %.6f below normal depth %.6f at xi=%v", nodes[i].Depth, yn, nodes[i].DistanceFromControl)
		}
	}
	if !p.UpstreamEnd.AtNormalDepth {
		t.Errorf("upstream end difference %.5f > 1 cm", p.UpstreamEnd.Difference)
	}
	if p.Reaches[0].CurveType != CurveBackwater {
		t.Errorf("curve = %q, want backwater", p.Reaches[0].CurveType)
	}
	if p.BackwaterExtentDistance == nil || *p.BackwaterExtentDistance > 2400 || *p.BackwaterExtentDistance < 1800 {
		v := -1.0
		if p.BackwaterExtentDistance != nil {
			v = *p.BackwaterExtentDistance
		}
		t.Errorf("backwater extent = %v, expected about 2 km (the 1 cm band entry)", v)
	}
}

// h=10 m must agree with h=20 m within a tight tolerance.
func TestStepRefinement(t *testing.T) {
	p20, _, _ := Compute(friendSpec(), Options{}, Hooks{})
	p10, _, _ := Compute(friendSpec(), Options{Step: 10}, Hooks{})
	for _, xi := range []float64{0, 500, 1000, 2000, 3000, 4000, 6000} {
		a, b := p20.DepthAt(xi), p10.DepthAt(xi)
		if math.Abs(a-b) > 2e-6 {
			t.Errorf("xi=%v: h20=%.7f h10=%.7f diff %.2e", xi, a, b, math.Abs(a-b))
		}
	}
}

// Case 2: control depth equal to normal depth gives the uniform profile.
func TestControlAtNormalDepth(t *testing.T) {
	spec := friendSpec()
	yn := mustNormal(t, spec)
	spec.Control = Control{Type: ControlTypeDepth, Depth: yn}
	p, fail, err := Compute(spec, Options{}, Hooks{})
	if err != nil || fail != nil {
		t.Fatalf("Compute: %v %+v", err, fail)
	}
	for _, pt := range p.depthNodes() {
		if math.Abs(pt.Depth-yn) > 2e-6 {
			t.Errorf("xi=%v depth %.6f != yn %.6f", pt.DistanceFromControl, pt.Depth, yn)
		}
	}
}

func mustNormal(t *testing.T, spec Spec) float64 {
	t.Helper()
	r := spec.Reaches[len(spec.Reaches)-1]
	yn, err := manning.NormalDepth(manning.Input{
		Section: r.Section, Roughness: r.Roughness, Slope: r.Slope, Flow: spec.Flow,
	})
	if err != nil {
		t.Fatal(err)
	}
	return yn
}

func mustCritical(t *testing.T, spec Spec) float64 {
	t.Helper()
	r := spec.Reaches[len(spec.Reaches)-1]
	yc, err := flow.CriticalDepth(r.Section, spec.Flow)
	if err != nil {
		t.Fatal(err)
	}
	return yc
}

// Case 3: M2 drawdown — yc < y_down < yn.
func TestM2Drawdown(t *testing.T) {
	spec := friendSpec()
	yn := mustNormal(t, spec)
	yc := mustCritical(t, spec)
	mid := (yn + yc) / 2
	spec.Control = Control{Type: ControlTypeDepth, Depth: mid}
	p, fail, err := Compute(spec, Options{}, Hooks{})
	if err != nil || fail != nil {
		t.Fatalf("Compute: %v %+v", err, fail)
	}
	if p.Reaches[0].CurveType != CurveDrawdown {
		t.Errorf("curve = %q, want drawdown", p.Reaches[0].CurveType)
	}
	nodes := p.depthNodes()
	for i := 1; i < len(nodes); i++ {
		if nodes[i].Depth < nodes[i-1].Depth-1e-9 {
			t.Fatalf("depth decreases upstream in an M2 push at xi=%v", nodes[i].DistanceFromControl)
		}
		if nodes[i].Depth > yn+1e-6 {
			t.Fatalf("depth %.6f crosses above yn %.6f", nodes[i].Depth, yn)
		}
	}
	if math.Abs(nodes[len(nodes)-1].Depth-yn) > 0.01 {
		t.Errorf("upstream depth %.4f not approaching yn %.4f", nodes[len(nodes)-1].Depth, yn)
	}
}

// Case 6a: downstream depth below critical must fail.
func TestControlBelowCriticalFails(t *testing.T) {
	spec := friendSpec()
	yc := mustCritical(t, spec)
	spec.Control = Control{Type: ControlTypeDepth, Depth: yc * 0.9}
	_, fail, err := Compute(spec, Options{}, Hooks{})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if fail == nil || fail.Code != FailureControlNotSubcritical {
		t.Fatalf("want %v failure, got %+v", FailureControlNotSubcritical, fail)
	}
}

// Case 6b: a steep reach in the chain fails naming that reach.
func TestSteepReachFails(t *testing.T) {
	spec := friendSpec()
	// Mild downstream reach, steep upstream reach.
	steep := ReachSpec{
		Name: "up-steep", Length: 1000,
		Section: geometry.Section{BottomWidth: 3}, Roughness: 0.012, Slope: 0.02,
	}
	spec.Reaches = append([]ReachSpec{steep}, spec.Reaches...)
	_, fail, err := Compute(spec, Options{}, Hooks{})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if fail == nil {
		t.Fatal("expected steep-reach failure")
	}
	if fail.ReachIndex != 0 || fail.Code != FailureSteepReach {
		t.Fatalf("failure = %+v", fail)
	}
}

// Case 6c: marching upstream, an abrupt narrowing pushes the flow to its
// critical depth at the reach seam — the job fails naming the reach and the
// distance, never returning a curve through Fr == 1.
func TestSeamCriticalCrossingFails(t *testing.T) {
	spec := friendSpec()
	spec.Reaches[0].Length = 3000
	narrow := ReachSpec{
		Name: "up-narrow", Length: 3000,
		Section: geometry.Section{BottomWidth: 1.4}, Roughness: 0.015, Slope: 0.001,
	}
	spec.Reaches = append([]ReachSpec{narrow}, spec.Reaches...)
	_, fail, err := Compute(spec, Options{}, Hooks{})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if fail == nil {
		t.Fatal("expected a failure at the narrowing")
	}
	if fail.Code != FailureControlNotSubcritical && fail.Code != FailureCriticalCrossing {
		t.Fatalf("unexpected failure code %q: %+v", fail.Code, fail)
	}
	if fail.ReachIndex != 0 {
		t.Errorf("reach = %d, want 0", fail.ReachIndex)
	}
	if math.Abs(fail.DistanceFromControl-3000) > 1 {
		t.Errorf("distance = %v, want 3000", fail.DistanceFromControl)
	}
}

// The general trapezoidal critical-depth solver must agree with the
// rectangular closed form.
func TestCriticalDepthAgreesWithClosedForm(t *testing.T) {
	for _, tc := range []struct {
		b, q float64
	}{{3, 5}, {2, 3}, {1.4, 5}, {6, 20}} {
		sec := geometry.Section{BottomWidth: tc.b}
		yc, err := flow.CriticalDepth(sec, tc.q)
		if err != nil {
			t.Fatal(err)
		}
		want := flow.RectangularCriticalDepth(tc.b, tc.q)
		if math.Abs(yc-want) > 1e-9 {
			t.Errorf("b=%v q=%v: bisect %.10f vs closed %.10f", tc.b, tc.q, yc, want)
		}
	}
}

// Trapezoidal reach with the same downstream weir still produces a
// monotone subcritical backwater and self-consistent Fr/V.
func TestTrapezoidalBackwater(t *testing.T) {
	spec := friendSpec()
	spec.Reaches[0].Section = geometry.Section{BottomWidth: 3, SideSlope: 1.5}
	p, fail, err := Compute(spec, Options{}, Hooks{})
	if err != nil || fail != nil {
		t.Fatalf("Compute: %v %+v", err, fail)
	}
	pts := p.depthNodes()
	for i := 1; i < len(pts); i++ {
		if pts[i].Depth > pts[i-1].Depth+1e-9 {
			t.Fatal("trapezoid depth increases upstream")
		}
		if pts[i].Froude >= 1 {
			t.Fatalf("xi=%v not subcritical: Fr=%v", pts[i].DistanceFromControl, pts[i].Froude)
		}
	}
	if !p.UpstreamEnd.AtNormalDepth {
		t.Errorf("head diff %v > 1 cm", p.UpstreamEnd.Difference)
	}
}

// Steep reach after a mild reach has been integrated: failure distance is
// at the seam (downstream reach length upstream of the control).
func TestSteepReachAfterMildReach(t *testing.T) {
	spec := friendSpec()
	down := spec.Reaches[0]
	down.Length = 2500
	down.Name = "down-mild"
	steep := ReachSpec{
		Name: "up-steep", Length: 2000,
		Section: geometry.Section{BottomWidth: 3}, Roughness: 0.012, Slope: 0.02,
	}
	spec.Reaches = []ReachSpec{steep, down}
	_, fail, err := Compute(spec, Options{}, Hooks{})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if fail == nil || fail.Code != FailureSteepReach || fail.ReachIndex != 0 {
		t.Fatalf("failure = %+v", fail)
	}
	if math.Abs(fail.DistanceFromControl-2500) > 1e-6 {
		t.Errorf("distance = %v, want 2500 (the seam)", fail.DistanceFromControl)
	}
}
