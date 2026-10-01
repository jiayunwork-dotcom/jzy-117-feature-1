package geometry

import (
	"math"
	"testing"
)

func TestRectangularSection(t *testing.T) {
	s := Section{BottomWidth: 2.0, SideSlope: 0}
	y := 1.5

	if got := s.Area(y); got != 3.0 {
		t.Errorf("Area = %v, want 3", got)
	}
	if got := s.WettedPerimeter(y); got != 5.0 {
		t.Errorf("WettedPerimeter = %v, want 5", got)
	}
	if got := s.TopWidth(y); got != 2.0 {
		t.Errorf("TopWidth = %v, want 2", got)
	}
	if got := s.HydraulicRadius(y); got != 0.6 {
		t.Errorf("HydraulicRadius = %v, want 0.6", got)
	}
}

func TestTrapezoidalSection(t *testing.T) {
	// b = 2, m = 1, y = 1:
	// A = (2+1)*1 = 3
	// P = 2 + 2*sqrt(2)
	// T = 2 + 2 = 4
	s := Section{BottomWidth: 2.0, SideSlope: 1.0}
	y := 1.0

	if got := s.Area(y); got != 3.0 {
		t.Errorf("Area = %v, want 3", got)
	}
	wantP := 2.0 + 2.0*math.Sqrt2
	if got := s.WettedPerimeter(y); math.Abs(got-wantP) > 1e-15 {
		t.Errorf("WettedPerimeter = %v, want %v", got, wantP)
	}
	if got := s.TopWidth(y); got != 4.0 {
		t.Errorf("TopWidth = %v, want 4", got)
	}
	wantR := 3.0 / wantP
	if got := s.HydraulicRadius(y); math.Abs(got-wantR) > 1e-15 {
		t.Errorf("HydraulicRadius = %v, want %v", got, wantR)
	}
}

func TestZeroDepth(t *testing.T) {
	s := Section{BottomWidth: 3.0, SideSlope: 2.0}
	if s.Area(0) != 0 || s.TopWidth(0) != 3.0 || s.HydraulicRadius(0) != 0 {
		t.Errorf("zero-depth geometry wrong: A=%v T=%v R=%v", s.Area(0), s.TopWidth(0), s.HydraulicRadius(0))
	}
}
