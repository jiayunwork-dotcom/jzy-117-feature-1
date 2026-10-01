package manning

import (
	"errors"
	"math"
	"testing"

	"openchannel/internal/geometry"
	"openchannel/internal/validation"
)

// rectInput is the pinned regression example:
// rectangular channel b = 2 m, n = 0.02 (SI), S0 = 0.001, Q = 3 m^3/s.
// Hand-checked order of magnitude: y ~ 1.37 m gives A ~ 2.74, R ~ 0.578,
// so (1/n)*A*R^(2/3)*sqrt(S) = 50*2.74*0.694*0.03162 ~= 3.0 m^3/s.
func rectInput() Input {
	return Input{
		Section:   geometry.Section{BottomWidth: 2.0, SideSlope: 0},
		Roughness: 0.02,
		Slope:     0.001,
		Flow:      3.0,
	}
}

func TestPinnedRectangularNormalDepth(t *testing.T) {
	yn, err := NormalDepth(rectInput())
	if err != nil {
		t.Fatalf("NormalDepth: %v", err)
	}
	// Independent high-precision reference value for the pinned example.
	const want = 1.3677515051663782
	if math.Abs(yn-want) > 1e-9 {
		t.Errorf("normal depth = %.12f, want %.12f", yn, want)
	}
	if !(yn > 1.3 && yn < 1.4) {
		t.Errorf("pinned normal depth %v outside hand-computed order of magnitude ~1.37 m", yn)
	}
}

// Core invariant: depth solved from Q must reproduce Q when put back into
// the same Manning equation and the same geometry functions.
func TestDepthRoundTripsToFlow(t *testing.T) {
	in := rectInput()
	yn, err := NormalDepth(in)
	if err != nil {
		t.Fatalf("NormalDepth: %v", err)
	}
	qBack := Discharge(in, yn)
	if rel := math.Abs(qBack-in.Flow) / in.Flow; rel > 1e-9 {
		t.Errorf("round-trip flow = %.12f, want %.12f (rel err %.2e)", qBack, in.Flow, rel)
	}
}

func TestTrapezoidalRoundTrip(t *testing.T) {
	in := Input{
		Section:   geometry.Section{BottomWidth: 3.0, SideSlope: 1.5},
		Roughness: 0.025,
		Slope:     0.0008,
	}
	// Generate the target flow from a known depth so the check is exact
	// regardless of hand computation.
	const target = 1.2
	in.Flow = Discharge(in, target)

	yn, err := NormalDepth(in)
	if err != nil {
		t.Fatalf("NormalDepth: %v", err)
	}
	if math.Abs(yn-target) > 1e-9 {
		t.Errorf("trapezoid depth = %.12f, want %.12f", yn, target)
	}
	if rel := math.Abs(Discharge(in, yn)-in.Flow) / in.Flow; rel > 1e-9 {
		t.Errorf("trapezoid round-trip rel err %.2e", rel)
	}
}

// Steeper bed slope must carry the same flow at a smaller normal depth.
func TestSteeperSlopeReducesDepth(t *testing.T) {
	base := rectInput()
	prev := math.Inf(1)
	for _, s := range []float64{0.001, 0.002, 0.005, 0.01} {
		in := base
		in.Slope = s
		yn, err := NormalDepth(in)
		if err != nil {
			t.Fatalf("NormalDepth(S=%v): %v", s, err)
		}
		if !(yn < prev) {
			t.Errorf("depth %v at S=%v not smaller than previous %v", yn, s, prev)
		}
		prev = yn
	}
}

func TestZeroFlowGivesZeroDepth(t *testing.T) {
	in := rectInput()
	in.Flow = 0
	yn, err := NormalDepth(in)
	if err != nil {
		t.Fatalf("NormalDepth: %v", err)
	}
	if yn != 0 {
		t.Errorf("zero-flow normal depth = %v, want 0", yn)
	}
}

// FrictionSlope must reproduce the bed slope at the normal depth, and
// Discharge based on that Sf must return the design flow: it is the same
// Manning relation inverted, not a second formula.
func TestFrictionSlopeAtNormalDepth(t *testing.T) {
	in := rectInput()
	yn, err := NormalDepth(in)
	if err != nil {
		t.Fatal(err)
	}
	sf := FrictionSlope(in.Section, in.Roughness, yn, in.Flow)
	if math.Abs(sf-in.Slope) > 1e-12 {
		t.Errorf("Sf(yn) = %.12f, want S0 = %.12f", sf, in.Slope)
	}
	// Round trip: Q from Sf via the same formula shape.
	a := in.Section.Area(yn)
	r := in.Section.HydraulicRadius(yn)
	qBack := a * math.Pow(r, 2.0/3.0) * math.Sqrt(sf) / in.Roughness
	if math.Abs(qBack-in.Flow) > 1e-9*in.Flow {
		t.Errorf("Q round trip via Sf = %v, want %v", qBack, in.Flow)
	}
	if sf := FrictionSlope(in.Section, in.Roughness, yn, 0); sf != 0 {
		t.Errorf("zero-flow Sf = %v, want 0", sf)
	}
}

func TestValidation(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*Input)
		field string
	}{
		{"non-positive roughness", func(in *Input) { in.Roughness = 0 }, "roughness"},
		{"negative roughness", func(in *Input) { in.Roughness = -0.02 }, "roughness"},
		{"non-positive slope", func(in *Input) { in.Slope = 0 }, "slope"},
		{"negative slope", func(in *Input) { in.Slope = -0.001 }, "slope"},
		{"non-positive bottom width", func(in *Input) { in.Section.BottomWidth = 0 }, "bottom_width"},
		{"negative bottom width", func(in *Input) { in.Section.BottomWidth = -2 }, "bottom_width"},
		{"negative side slope", func(in *Input) { in.Section.SideSlope = -1 }, "side_slope"},
		{"negative flow", func(in *Input) { in.Flow = -1 }, "flow"},
		{"NaN roughness", func(in *Input) { in.Roughness = math.NaN() }, "roughness"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := rectInput()
			tc.mut(&in)
			_, err := NormalDepth(in)
			var ve *validation.Error
			if !errors.As(err, &ve) {
				t.Fatalf("want *validation.Error, got %v", err)
			}
			if ve.Field != tc.field {
				t.Errorf("error field = %q, want %q", ve.Field, tc.field)
			}
		})
	}
}
