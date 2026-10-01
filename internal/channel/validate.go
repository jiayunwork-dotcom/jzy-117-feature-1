package channel

import (
	"fmt"

	"openchannel/internal/validation"
	"openchannel/internal/weir"
)

// Validate checks the whole definition before it is stored. Field paths name
// the offending element, e.g. "reaches[2].roughness", so the HTTP layer can
// report exactly what was wrong.
func (d Definition) Validate() error {
	if len(d.Reaches) == 0 {
		return fail("reaches", "at least one reach is required")
	}
	if err := validation.Positive("design_flow", d.DesignFlow); err != nil {
		return err
	}
	for i, r := range d.Reaches {
		prefix := fmt.Sprintf("reaches[%d]", i)
		if err := validation.Positive(prefix+".length", r.Length); err != nil {
			return err
		}
		if err := validation.Positive(prefix+".bottom_width", r.Section.BottomWidth); err != nil {
			return err
		}
		if err := validation.NonNegative(prefix+".side_slope", r.Section.SideSlope); err != nil {
			return err
		}
		if err := validation.Positive(prefix+".roughness", r.Roughness); err != nil {
			return err
		}
		if err := validation.Positive(prefix+".slope", r.Slope); err != nil {
			return err
		}
	}
	switch d.Control.Kind {
	case ControlWeir:
		if err := validation.Positive("control.width", d.Control.Width); err != nil {
			return err
		}
		if err := validation.NonNegative("control.crest_height", d.Control.CrestHeight); err != nil {
			return err
		}
		if err := validation.Positive("control.discharge_coefficient", d.Control.Cd); err != nil {
			return err
		}
	case ControlDepth:
		if err := validation.Positive("control.depth", d.Control.Depth); err != nil {
			return err
		}
	default:
		return fail("control.kind", "must be %q or %q", ControlWeir, ControlDepth)
	}
	return nil
}

// DownstreamDepth resolves the boundary water depth at the foot of the last
// reach from the control. For a weir it inverts the existing weir discharge
// relation and adds the crest height; for a prescribed depth it echoes it.
//
// It uses weir.Head exclusively — there is no second copy of the weir law.
func (d Definition) DownstreamDepth() (float64, error) {
	switch d.Control.Kind {
	case ControlDepth:
		return d.Control.Depth, nil
	case ControlWeir:
		head, err := weir.Head(weir.FlowInput{
			Width:                d.Control.Width,
			Flow:                 d.DesignFlow,
			DischargeCoefficient: d.Control.Cd,
		})
		if err != nil {
			return 0, err
		}
		return d.Control.CrestHeight + head, nil
	default:
		return 0, fmt.Errorf("unknown control kind %q", d.Control.Kind)
	}
}

// fail returns a validation-style error with the same field/message shape used
// by the other hydraulic packages.
func fail(field, format string, args ...any) error {
	return &validation.Error{Field: field, Message: fmt.Sprintf(format, args...)}
}
