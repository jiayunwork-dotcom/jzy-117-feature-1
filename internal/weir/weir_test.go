package weir

import (
	"errors"
	"math"
	"testing"

	"openchannel/internal/validation"
)

func TestDischargeValueAndPowerLaw(t *testing.T) {
	// b = 0.8 m, H = 0.25 m, Cd = 0.62
	// Q = (2/3)*0.62*0.8*sqrt(2*9.81)*0.25^1.5 ~= 0.1831 m^3/s
	q, err := Discharge(Input{Width: 0.8, Head: 0.25, DischargeCoefficient: 0.62})
	if err != nil {
		t.Fatalf("Discharge: %v", err)
	}
	const want = 0.1830838059468942
	if math.Abs(q-want) > 1e-10 {
		t.Errorf("weir Q = %.12f, want %.12f", q, want)
	}

	// At fixed Cd and width, raising the head scales Q as H^(3/2).
	base, _ := Discharge(Input{Width: 0.8, Head: 0.1, DischargeCoefficient: 0.62})
	raised, _ := Discharge(Input{Width: 0.8, Head: 0.25, DischargeCoefficient: 0.62})
	ratio := raised / base
	wantRatio := math.Pow(0.25/0.1, 1.5)
	if math.Abs(ratio-wantRatio) > 1e-12 {
		t.Errorf("head scaling ratio = %.12f, want %.12f", ratio, wantRatio)
	}
	if raised <= base {
		t.Error("larger head must yield larger discharge")
	}
}

func TestValidation(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*Input)
		field string
	}{
		{"non-positive width", func(in *Input) { in.Width = 0 }, "width"},
		{"negative width", func(in *Input) { in.Width = -0.8 }, "width"},
		{"non-positive head", func(in *Input) { in.Head = 0 }, "head"},
		{"negative head", func(in *Input) { in.Head = -0.2 }, "head"},
		{"non-positive coefficient", func(in *Input) { in.DischargeCoefficient = 0 }, "discharge_coefficient"},
		{"NaN head", func(in *Input) { in.Head = math.NaN() }, "head"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := Input{Width: 0.8, Head: 0.25, DischargeCoefficient: DefaultDischargeCoefficient}
			tc.mut(&in)
			_, err := Discharge(in)
			var ve *validation.Error
			if !errors.As(err, &ve) {
				t.Fatalf("want *validation.Error, got %v", err)
			}
			if ve.Field != tc.field {
				t.Errorf("field = %q, want %q", ve.Field, tc.field)
			}
		})
	}
}
