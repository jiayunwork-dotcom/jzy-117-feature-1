// Package geometry defines the open-channel cross section and the single set
// of geometric functions (area, wetted perimeter, water-surface/top width and
// hydraulic radius).
//
// Every other package must derive geometric quantities from here so that the
// Manning solver and the Froude-number/regime logic can never disagree about
// section shape: there is exactly one implementation of A, P and T.
//
// A trapezoidal section is described by bottom width b and side slope m
// (horizontal run per vertical rise). A rectangular section is the special
// case m == 0.
package geometry

import "math"

// Section is a trapezoidal (m > 0) or rectangular (m == 0) cross section.
type Section struct {
	BottomWidth float64 // b, in metres
	SideSlope   float64 // m, horizontal per vertical; 0 for a rectangular section
}

// Area returns the wetted cross-sectional area for depth y:
//
//	A = (b + m*y) * y
func (s Section) Area(y float64) float64 {
	return (s.BottomWidth + s.SideSlope*y) * y
}

// WettedPerimeter returns the wetted perimeter for depth y:
//
//	P = b + 2*y*sqrt(1 + m^2)
func (s Section) WettedPerimeter(y float64) float64 {
	return s.BottomWidth + 2.0*y*math.Sqrt(1.0+s.SideSlope*s.SideSlope)
}

// TopWidth returns the water-surface width for depth y:
//
//	T = b + 2*m*y
//
// This is the width used by the Froude-number calculation.
func (s Section) TopWidth(y float64) float64 {
	return s.BottomWidth + 2.0*s.SideSlope*y
}

// HydraulicRadius returns R = A / P, with both terms taken from this package.
func (s Section) HydraulicRadius(y float64) float64 {
	p := s.WettedPerimeter(y)
	if p == 0 {
		return 0
	}
	return s.Area(y) / p
}
