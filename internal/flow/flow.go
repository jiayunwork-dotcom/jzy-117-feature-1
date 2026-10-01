// Package flow derives velocity, Froude number and flow regime for an open
// channel. The surface (top) width used here comes from the same geometry
// functions used by the Manning solver, so A and T can never come from two
// diverging section implementations.
package flow

import (
	"errors"
	"fmt"
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

// criticalMaxIterations bounds the critical-depth bisection; the bracket
// shrinks by 2^-iterations, far beyond engineering precision.
const criticalMaxIterations = 200

// ErrCriticalDepthDidNotConverge is returned if the Fr == 1 root cannot be
// bracketed (effectively unreachable for finite, valid, positive-flow inputs).
var ErrCriticalDepthDidNotConverge = errors.New("flow: critical-depth iteration did not converge")

// CriticalDepth solves the general critical depth for a trapezoidal or
// rectangular section, defined by Fr == 1:
//
//	Q^2 * T(y) = g * A(y)^3
//
// Discharge is strictly increasing with depth while the left/right ratio
// Q^2*T/(g*A^3) is strictly decreasing, so the root is bracketed starting
// from a tiny depth and located by bisection. The geometry (A, T) and the
// gravity constant come from this same package: this is not a second copy
// of the critical-depth relation, and for a rectangular section it agrees
// with RectangularCriticalDepth to bisection precision. Q == 0 has no
// positive critical depth and returns 0.
func CriticalDepth(sec geometry.Section, q float64) (float64, error) {
	if q <= 0 {
		return 0, nil
	}
	// f(y) > 0 means supercritical side (y < yc), f(y) < 0 means deep.
	f := func(y float64) float64 {
		a := sec.Area(y)
		t := sec.TopWidth(y)
		return q*q*t/(g*a*a*a) - 1.0
	}

	lo := 1e-9
	for range 100 {
		if f(lo) < 0 {
			lo *= 0.5
			continue
		}
		break
	}
	hi := math.Max(lo*2, 1.0)
	for range 200 {
		if f(hi) <= 0 {
			break
		}
		hi *= 2
	}
	if f(hi) > 0 {
		return 0, fmt.Errorf("%w: flow %.6g beyond searchable depth", ErrCriticalDepthDidNotConverge, q)
	}

	const tol = 1e-11
	for i := 0; i < criticalMaxIterations; i++ {
		mid := (lo + hi) / 2
		if f(mid) > 0 {
			lo = mid
		} else {
			hi = mid
		}
		if hi-lo <= tol*math.Max(1.0, hi) {
			return (lo + hi) / 2, nil
		}
	}
	return (lo + hi) / 2, fmt.Errorf("%w after %d iterations", ErrCriticalDepthDidNotConverge, criticalMaxIterations)
}
