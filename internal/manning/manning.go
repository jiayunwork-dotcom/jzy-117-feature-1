// Package manning solves uniform open-channel flow with Manning's equation.
//
// SI units are used throughout, so
//
//	Q = (1/n) * A * R^(2/3) * S0^(1/2)
//
// with A, R taken exclusively from the geometry package. The inverse problem
// (known Q, unknown normal depth) is solved by bisection, which is safe here
// because discharge is strictly increasing with depth for any valid section.
package manning

import (
	"errors"
	"fmt"
	"math"

	"openchannel/internal/geometry"
	"openchannel/internal/validation"
)

// Input describes one uniform-flow computation.
type Input struct {
	Section   geometry.Section
	Roughness float64 // Manning's n (SI), must be > 0
	Slope     float64 // channel-bottom slope S0, must be > 0
	Flow      float64 // discharge Q in m^3/s, must be >= 0
}

// Validate enforces n > 0, S0 > 0, b > 0, m >= 0 and Q >= 0.
func (in Input) Validate() error {
	if err := validation.Positive("bottom_width", in.Section.BottomWidth); err != nil {
		return err
	}
	if err := validation.NonNegative("side_slope", in.Section.SideSlope); err != nil {
		return err
	}
	if err := validation.Positive("roughness", in.Roughness); err != nil {
		return err
	}
	if err := validation.Positive("slope", in.Slope); err != nil {
		return err
	}
	return validation.NonNegative("flow", in.Flow)
}

const (
	// bracketMaxDoublings caps how many times the initial [0, 1] m depth
	// bracket is doubled while searching for an upper bound.
	bracketMaxDoublings = 200
	// maxIterations bounds the bisection. Interval shrinks by 2^-iterations,
	// far tighter than the requested relative tolerance.
	maxIterations = 200
)

// ErrDidNotConverge is returned if the root cannot be bracketed or located
// (effectively unreachable for finite, valid inputs).
var ErrDidNotConverge = errors.New("manning: normal-depth iteration did not converge")

// Discharge evaluates Manning's equation at depth y.
func Discharge(in Input, y float64) float64 {
	a := in.Section.Area(y)
	r := in.Section.HydraulicRadius(y)
	return (1.0 / in.Roughness) * a * math.Pow(r, 2.0/3.0) * math.Sqrt(in.Slope)
}

// FrictionSlope inverts Manning's equation for the energy (friction) slope
// Sf at a known depth and discharge:
//
//	Q = (1/n) * A * R^(2/3) * sqrt(Sf)  =>  Sf = (n*Q / (A*R^(2/3)))^2
//
// It uses the same geometry functions and the same n/Q relation as
// Discharge, so the gradually-varied-flow engine never carries a second
// copy of Manning's formula. At the normal depth the returned value equals
// the bed slope S0; zero wetted area yields +Inf (dry bed).
func FrictionSlope(sec geometry.Section, roughness, depth, q float64) float64 {
	a := sec.Area(depth)
	if a <= 0 {
		return math.Inf(1)
	}
	r := sec.HydraulicRadius(depth)
	sqrtSf := roughness * q / (a * math.Pow(r, 2.0/3.0))
	return sqrtSf * sqrtSf
}

// NormalDepth inverts Manning's equation for the normal depth. The zero-flow
// case returns depth 0 directly; otherwise the flow root is bracketed from
// above (discharge is monotonic in depth) and bisected to machine precision.
func NormalDepth(in Input) (float64, error) {
	if err := in.Validate(); err != nil {
		return 0, err
	}
	if in.Flow == 0 {
		return 0, nil
	}

	lo := 0.0
	hi := 1.0
	// Double the bracket until discharge at hi exceeds the target.
	for range bracketMaxDoublings {
		if Discharge(in, hi) >= in.Flow {
			break
		}
		lo = hi
		hi *= 2
	}
	if Discharge(in, hi) < in.Flow {
		return 0, fmt.Errorf("%w: flow %.6g beyond searchable depth", ErrDidNotConverge, in.Flow)
	}

	// Bisect. The relative tolerance below matches the regression tests'
	// round-trip tolerance at very small target depths.
	const tol = 1e-11
	for i := 0; i < maxIterations; i++ {
		mid := (lo + hi) / 2
		if Discharge(in, mid) < in.Flow {
			lo = mid
		} else {
			hi = mid
		}
		if hi-lo <= tol*math.Max(1.0, hi) {
			return (lo + hi) / 2, nil
		}
	}
	return (lo + hi) / 2, fmt.Errorf("%w after %d iterations", ErrDidNotConverge, maxIterations)
}
