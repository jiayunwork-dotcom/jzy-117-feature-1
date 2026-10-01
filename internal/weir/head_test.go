package weir

import (
	"errors"
	"math"
	"testing"

	"openchannel/internal/validation"
)

// TestHeadInvertsDischarge: Head must be the exact inverse of Discharge - the
// profile downstream boundary relies on this one relation.
func TestHeadInvertsDischarge(t *testing.T) {
	for _, q := range []float64{0.183, 1.0, 5.0, 42.0} {
		in := FlowInput{Width: 3, Flow: q, DischargeCoefficient: 0.62}
		h, err := Head(in)
		if err != nil {
			t.Fatal(err)
		}
		qBack, err := Discharge(Input{Width: 3, Head: h, DischargeCoefficient: 0.62})
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(qBack-q)/q > 1e-12 {
			t.Errorf("Q %.6g round-trip: got %.12g", q, qBack)
		}
	}

	// Case I pinned value: Q = 5 through b = 3, Cd = 0.62 gives H ~= 0.939 m,
	// hence depth over a 1.0 m crest ~= 1.939 m.
	h, err := Head(FlowInput{Width: 3, Flow: 5, DischargeCoefficient: 0.62})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(h-0.939) > 0.001 {
		t.Errorf("head for pinned case = %.6f, want ~0.939 m", h)
	}
}

func TestHeadZeroFlowAndValidation(t *testing.T) {
	h, err := Head(FlowInput{Width: 3, Flow: 0, DischargeCoefficient: 0.62})
	if err != nil || h != 0 {
		t.Fatalf("zero flow: h=%v err=%v", h, err)
	}
	for _, tc := range []struct {
		mut   func(*FlowInput)
		field string
	}{
		{func(in *FlowInput) { in.Width = 0 }, "width"},
		{func(in *FlowInput) { in.Flow = -1 }, "flow"},
		{func(in *FlowInput) { in.DischargeCoefficient = 0 }, "discharge_coefficient"},
	} {
		in := FlowInput{Width: 3, Flow: 5, DischargeCoefficient: DefaultDischargeCoefficient}
		tc.mut(&in)
		_, err := Head(in)
		var ve *validation.Error
		if !errors.As(err, &ve) || ve.Field != tc.field {
			t.Errorf("got %v, want validation error on %q", err, tc.field)
		}
	}
}
