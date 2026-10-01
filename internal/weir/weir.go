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
	return discharge(in.Width, in.Head, in.DischargeCoefficient), nil
}

// discharge is the single evaluation of the weir power law. Head (the inverse)
// inverts exactly this expression, so the head/discharge relation can never
// come from two independent implementations.
func discharge(width, head, cd float64) float64 {
	return (2.0 / 3.0) * cd * width * math.Sqrt(2.0*g) * math.Pow(head, 1.5)
}

// FlowInput asks for the head H that passes flow Q through a weir of the given
// width and discharge coefficient.
type FlowInput struct {
	Width                float64 // weir width b, metres, must be > 0
	Flow                 float64 // discharge Q in m^3/s, must be >= 0
	DischargeCoefficient float64 // Cd, must be > 0; use DefaultDischargeCoefficient if unsure
}

// Validate enforces b > 0, Q >= 0, Cd > 0.
func (in FlowInput) Validate() error {
	if err := validation.Positive("width", in.Width); err != nil {
		return err
	}
	if err := validation.NonNegative("flow", in.Flow); err != nil {
		return err
	}
	return validation.Positive("discharge_coefficient", in.DischargeCoefficient)
}

// Head inverts Discharge:
//
//	H = ( 3*Q / (2*Cd*b*sqrt(2*g)) )^(2/3)
//
// It is the only inverse of the weir relation. Q == 0 gives H == 0.
func Head(in FlowInput) (float64, error) {
	if err := in.Validate(); err != nil {
		return 0, err
	}
	if in.Flow == 0 {
		return 0, nil
	}
	k := (2.0 / 3.0) * in.DischargeCoefficient * in.Width * math.Sqrt(2.0*g)
	return math.Pow(in.Flow/k, 2.0/3.0), nil
}
