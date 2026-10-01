// Package weir computes discharge over a sharp-crested rectangular weir with
// the standard formula (no velocity-of-approach correction):
//
//	Q = (2/3) * Cd * b * sqrt(2*g) * H^(3/2)
//
// At fixed Cd the discharge grows as the 3/2 power of the head.
package weir

import (
	"math"

	"openchannel/internal/validation"
)

const g = 9.81

// DefaultDischargeCoefficient is used when a request omits Cd.
const DefaultDischargeCoefficient = 0.62

// Input describes one weir calculation.
type Input struct {
	Width                float64 // weir width b, metres, must be > 0
	Head                 float64 // head over the weir H, metres, must be > 0
	DischargeCoefficient float64 // Cd, must be > 0; use DefaultDischargeCoefficient if unsure
}

// Validate enforces b > 0, H > 0, Cd > 0.
func (in Input) Validate() error {
	if err := validation.Positive("width", in.Width); err != nil {
		return err
	}
	if err := validation.Positive("head", in.Head); err != nil {
		return err
	}
	return validation.Positive("discharge_coefficient", in.DischargeCoefficient)
}

// Discharge evaluates the rectangular-weir formula.
func Discharge(in Input) (float64, error) {
	if err := in.Validate(); err != nil {
		return 0, err
	}
	return (2.0 / 3.0) * in.DischargeCoefficient * in.Width *
		math.Sqrt(2.0*g) * math.Pow(in.Head, 1.5), nil
}

// Head inverts the rectangular-weir formula for the head over the crest
// required to pass a known discharge:
//
//	H = (Q / ((2/3) * Cd * b * sqrt(2*g)))^(2/3)
//
// It is the exact inverse of Discharge with the same Cd/b constants, so the
// downstream water-level control for a profile push is derived from the one
// weir relation this service has — never from a re-derived copy.
func Head(width, dischargeCoefficient, q float64) (float64, error) {
	if err := validation.Positive("width", width); err != nil {
		return 0, err
	}
	if err := validation.Positive("discharge_coefficient", dischargeCoefficient); err != nil {
		return 0, err
	}
	if q < 0 {
		return 0, &validation.Error{Field: "flow", Message: "must be greater than or equal to 0"}
	}
	if q == 0 {
		return 0, nil
	}
	coeff := (2.0 / 3.0) * dischargeCoefficient * width * math.Sqrt(2.0*g)
	return math.Pow(q/coeff, 2.0/3.0), nil
}
