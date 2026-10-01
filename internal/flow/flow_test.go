package flow

import (
	"math"
	"testing"

	"openchannel/internal/geometry"
)

// The closed-form rectangular critical depth yc = (Q^2/(g*b^2))^(1/3) is the
// independent anchor: at y = yc the shared Froude number must be 1, above it
// subcritical, below it supercritical.
func TestRectangularCriticalDepthAnchorsRegime(t *testing.T) {
	sec := geometry.Section{BottomWidth: 2.0, SideSlope: 0}
	q := 3.0

	yc := RectangularCriticalDepth(sec.BottomWidth, q)
	// Direct closed-form value, SI g = 9.81.
	wantYC := math.Cbrt(q * q / (9.81 * 4.0))
	if math.Abs(yc-wantYC) > 1e-12 {
		t.Fatalf("yc = %.12f, want %.12f", yc, wantYC)
	}

	atCritical := AtDepth(sec, yc, q)
	if math.Abs(atCritical.Froude-1) > 1e-9 {
		t.Errorf("Fr(yc) = %.12f, want 1", atCritical.Froude)
	}
	if r := Regime(atCritical.Froude); r != RegimeCritical {
		t.Errorf("regime at yc = %q, want critical", r)
	}

	// Top width used by Fr is the geometry package's rectangular surface width.
	if atCritical.TopWidth != sec.BottomWidth {
		t.Errorf("top width = %v, want bottom width %v", atCritical.TopWidth, sec.BottomWidth)
	}

	deep := AtDepth(sec, yc*1.5, q)
	if deep.Froude >= 1 || Regime(deep.Froude) != RegimeSubcritical {
		t.Errorf("deep regime wrong: Fr=%v regime=%q", deep.Froude, Regime(deep.Froude))
	}
	shallow := AtDepth(sec, yc*0.5, q)
	if shallow.Froude <= 1 || Regime(shallow.Froude) != RegimeSupercritical {
		t.Errorf("shallow regime wrong: Fr=%v regime=%q", shallow.Froude, Regime(shallow.Froude))
	}
}

// Pinned uniform-flow example (b = 2, n = 0.02, S0 = 0.001, Q = 3): its normal
// depth 1.3678 m is well above yc ~ 0.6121 m, hence mild/subcritical.
func TestPinnedExampleRegime(t *testing.T) {
	sec := geometry.Section{BottomWidth: 2.0, SideSlope: 0}
	q := 3.0
	const yn = 1.3677515051663782

	got := AtDepth(sec, yn, q)
	if math.Abs(got.Velocity-1.0966904399915354) > 1e-9 {
		t.Errorf("V = %.12f, want 1.0966904399915354", got.Velocity)
	}
	if math.Abs(got.Froude-0.29939597210037816) > 1e-9 {
		t.Errorf("Fr = %.12f, want 0.2993959721", got.Froude)
	}
	if Regime(got.Froude) != RegimeSubcritical {
		t.Errorf("regime = %q, want subcritical", Regime(got.Froude))
	}
}

// Same consistency check for a trapezoid: A and T must both trace back to the
// geometry functions, and Froude evaluated at yc-trapezoid stays self-consistent.
func TestTrapezoidConsistency(t *testing.T) {
	sec := geometry.Section{BottomWidth: 3.0, SideSlope: 1.5}
	y := 1.2
	q := 4.0
	got := AtDepth(sec, y, q)

	wantA := sec.Area(y)
	wantT := sec.TopWidth(y)
	if got.Area != wantA || got.TopWidth != wantT {
		t.Errorf("geometry mismatch: A got %v want %v, T got %v want %v",
			got.Area, wantA, got.TopWidth, wantT)
	}
	wantV := q / wantA
	wantFr := wantV / math.Sqrt(9.81*wantA/wantT)
	if math.Abs(got.Velocity-wantV) > 1e-12 || math.Abs(got.Froude-wantFr) > 1e-12 {
		t.Errorf("V/Fr inconsistent with shared geometry: got (%v,%v) want (%v,%v)",
			got.Velocity, got.Froude, wantV, wantFr)
	}
}

func TestRegimeLabels(t *testing.T) {
	cases := map[float64]string{
		0:     RegimeNoFlow,
		0.5:   RegimeSubcritical,
		1:     RegimeCritical,
		1.001: RegimeSupercritical,
	}
	for fr, want := range cases {
		if got := Regime(fr); got != want {
			t.Errorf("Regime(%v) = %q, want %q", fr, got, want)
		}
	}
}
