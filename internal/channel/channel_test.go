package channel

import (
	"errors"
	"math"
	"testing"

	"openchannel/internal/geometry"
	"openchannel/internal/validation"
)

func goodDef() Definition {
	return Definition{
		Reaches: []Reach{{
			Length:    1000,
			Section:   geometry.Section{BottomWidth: 3, SideSlope: 0},
			Roughness: 0.015,
			Slope:     0.001,
		}},
		DesignFlow: 5,
		Control: Control{
			Kind: ControlWeir, Width: 3, CrestHeight: 1.0, Cd: 0.62,
		},
	}
}

func TestDefinitionValidate(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*Definition)
		field string
	}{
		{"no reaches", func(d *Definition) { d.Reaches = nil }, "reaches"},
		{"zero flow", func(d *Definition) { d.DesignFlow = 0 }, "design_flow"},
		{"zero reach length", func(d *Definition) { d.Reaches[0].Length = 0 }, "reaches[0].length"},
		{"bad width", func(d *Definition) { d.Reaches[0].Section.BottomWidth = -1 }, "reaches[0].bottom_width"},
		{"negative side slope", func(d *Definition) { d.Reaches[0].Section.SideSlope = -2 }, "reaches[0].side_slope"},
		{"bad roughness", func(d *Definition) { d.Reaches[0].Roughness = 0 }, "reaches[0].roughness"},
		{"bad slope", func(d *Definition) { d.Reaches[0].Slope = -0.01 }, "reaches[0].slope"},
		{"unknown control", func(d *Definition) { d.Control.Kind = "flume" }, "control.kind"},
		{"weir zero width", func(d *Definition) { d.Control.Width = 0 }, "control.width"},
		{"weir negative crest", func(d *Definition) { d.Control.CrestHeight = -0.1 }, "control.crest_height"},
		{"weir zero cd", func(d *Definition) { d.Control.Cd = 0 }, "control.discharge_coefficient"},
		{"depth zero", func(d *Definition) {
			d.Control = Control{Kind: ControlDepth, Depth: 0}
		}, "control.depth"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := goodDef()
			tc.mut(&d)
			err := d.Validate()
			var ve *validation.Error
			if !errors.As(err, &ve) {
				t.Fatalf("want validation error, got %v", err)
			}
			if ve.Field != tc.field {
				t.Errorf("field = %q, want %q", ve.Field, tc.field)
			}
		})
	}
}

func TestValidDefinitionPasses(t *testing.T) {
	if err := goodDef().Validate(); err != nil {
		t.Fatalf("good definition rejected: %v", err)
	}
	d := goodDef()
	d.Control = Control{Kind: ControlDepth, Depth: 1.5}
	if err := d.Validate(); err != nil {
		t.Fatalf("depth control rejected: %v", err)
	}
}

func TestDistances(t *testing.T) {
	d := Definition{Reaches: []Reach{
		{Length: 1000}, {Length: 2000}, {Length: 3000},
	}}
	if d.TotalLength() != 6000 {
		t.Errorf("total = %v", d.TotalLength())
	}
	// reach 0 upstream end at 6000, downstream end at 5000; reach 2 is at 0..3000.
	if got := d.ReachDownDistance(0); got != 5000 {
		t.Errorf("reach0 down = %v, want 5000", got)
	}
	if got := d.ReachUpDistance(0); got != 6000 {
		t.Errorf("reach0 up = %v, want 6000", got)
	}
	if got := d.ReachDownDistance(2); got != 0 {
		t.Errorf("reach2 down = %v, want 0", got)
	}
	if got := d.ReachUpDistance(2); got != 3000 {
		t.Errorf("reach2 up = %v, want 3000", got)
	}
}

// TestDownstreamDepthUsesWeirLaw: the pinned weir case gives ~1.939 m depth
// (crest 1.0 + head ~0.939), proving channel reuse of weir.Head.
func TestDownstreamDepthUsesWeirLaw(t *testing.T) {
	d, err := goodDef().DownstreamDepth()
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(d-1.939) > 0.001 {
		t.Errorf("downstream depth = %.4f, want ~1.939", d)
	}
	d2 := goodDef()
	d2.Control = Control{Kind: ControlDepth, Depth: 1.7}
	y, err := d2.DownstreamDepth()
	if err != nil || y != 1.7 {
		t.Errorf("depth control: y=%v err=%v", y, err)
	}
}
