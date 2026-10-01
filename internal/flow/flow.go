// Package flow derives velocity, Froude number and flow regime for an open
// channel. The surface (top) width used here comes from the same geometry
// functions used by the Manning solver, so A and T can never come from two
// diverging section implementations.
package flow

import (
	"math"

	"openchannel/internal/geometry"
)

// Gravity acceleration in m/s^2, SI units.
const g = 9.81

// Regime labels.
const (
	RegimeSubcritical   = "subcritical"
	RegimeCritical      = "critical"
	RegimeSupercritical = "supercritical"
	RegimeNoFlow        = "no_flow"
)

// Quantities bundles the values derived from a section and a depth.
type Quantities struct {
	Area            float64
	TopWidth        float64
	HydraulicRadius float64
	Velocity        float64
	Froude          float64
}

// AtDepth computes mean velocity V = Q/A, Froude number
//
//	Fr = V / sqrt(g * A / T)
//
// and reports the geometry used, all from the geometry package. Q == 0 yields
// zero velocity and zero Froude number.
func AtDepth(sec geometry.Section, depth, q float64) Quantities {
	a := sec.Area(depth)
	t := sec.TopWidth(depth)
	r := sec.HydraulicRadius(depth)

	var v, fr float64
	if a > 0 {
		v = q / a
		denom := g * a / t
		if denom > 0 {
			fr = v / math.Sqrt(denom)
		}
	}
	return Quantities{
		Area:            a,
		TopWidth:        t,
		HydraulicRadius: r,
		Velocity:        v,
		Froude:          fr,
	}
}

// Regime classifies a Froude number. Fr == 0 (no flow) is reported separately;
// a narrow band around 1 is treated as critical.
func Regime(fr float64) string {
	switch {
	case math.IsNaN(fr):
		return "undefined"
	case fr == 0:
		return RegimeNoFlow
	case math.Abs(fr-1) <= 1e-9:
		return RegimeCritical
	case fr < 1:
		return RegimeSubcritical
	default:
		return RegimeSupercritical
	}
}

// RectangularCriticalDepth is the closed-form critical depth for a rectangular
// channel:
//
//	yc = (Q^2 / (g * b^2))^(1/3)
//
// It serves both as an engineering convenience and as an independent check
// that the Froude-based regime classification is correct.
func RectangularCriticalDepth(bottomWidth, q float64) float64 {
	return math.Cbrt(q * q / (g * bottomWidth * bottomWidth))
}
