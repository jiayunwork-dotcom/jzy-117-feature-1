package flow

import (
	"math"
	"testing"

	"openchannel/internal/geometry"
)

// TestCriticalDepthGeneralSection: the general critical-depth solver must (a)
// reproduce the rectangular closed form and (b) find Fr == 1 for a trapezoid,
// using the same geometry/AtDepth functions the profile engine uses.
func TestCriticalDepthGeneralSection(t *testing.T) {
	// Rectangular case must agree with the closed form.
	rect := geometry.Section{BottomWidth: 3}
	for _, q := range []float64{1.0, 5.0, 20.0} {
		yc := CriticalDepth(rect, q)
		want := RectangularCriticalDepth(3, q)
		if math.Abs(yc-want) > 1e-10 {
			t.Errorf("rect Q=%v: yc %.10f != closed form %.10f", q, yc, want)
		}
		if got := AtDepth(rect, yc, q).Froude; math.Abs(got-1) > 1e-9 {
			t.Errorf("Fr(yc) = %.12f, want 1", got)
		}
	}

	// Trapezoid: no closed form, but the defining condition Fr == 1 must hold
	// and the depth must be smaller than the rectangular value (wider water
	// surface at the same area carries critical flow at smaller depth).
	trap := geometry.Section{BottomWidth: 3, SideSlope: 1.5}
	q := 10.0
	yc := CriticalDepth(trap, q)
	if yc <= 0 {
		t.Fatalf("non-positive yc %v", yc)
	}
	if got := AtDepth(trap, yc, q).Froude; math.Abs(got-1) > 1e-9 {
		t.Errorf("trapezoid Fr(yc) = %.12f, want 1", got)
	}
	if yc >= RectangularCriticalDepth(3, q) {
		t.Errorf("trapezoid yc %.6f expected below rectangular value", yc)
	}
	// Subcritical above, supercritical below.
	if AtDepth(trap, yc*1.3, q).Froude >= 1 {
		t.Error("expected subcritical above yc")
	}
	if AtDepth(trap, yc*0.7, q).Froude <= 1 {
		t.Error("expected supercritical below yc")
	}

	// Zero flow.
	if CriticalDepth(rect, 0) != 0 {
		t.Error("zero-flow critical depth must be 0")
	}
}
