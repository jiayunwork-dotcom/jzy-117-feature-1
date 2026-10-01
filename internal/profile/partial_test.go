package profile

import (
	"math"
	"testing"

	"openchannel/internal/channel"
	"openchannel/internal/geometry"
)

func twoReachDef(upstreamN float64) channel.Definition {
	return channel.Definition{
		Reaches: []channel.Reach{
			{
				Name:      "upper",
				Length:    2000,
				Section:   geometry.Section{BottomWidth: 3},
				Roughness: upstreamN,
				Slope:     0.001,
			},
			{
				Name:      "lower",
				Length:    3000,
				Section:   geometry.Section{BottomWidth: 3},
				Roughness: 0.015,
				Slope:     0.001,
			},
		},
		DesignFlow: 5,
		Control: channel.Control{
			Kind:        channel.ControlWeir,
			Width:       3,
			CrestHeight: 1.0,
			Cd:          0.62,
		},
	}
}

// TestPartialEligibility covers the rule table.
func TestPartialEligibility(t *testing.T) {
	old := twoReachDef(0.015)

	// Same: not a change, but formally the whole tail matches.
	if first, ok := PartialEligible(old, twoReachDef(0.015)); !ok || first != 0 {
		t.Errorf("identical defs: first=%d ok=%v, want 0,true", first, ok)
	}

	// Upstream roughness changed: suffix of one reach reusable.
	if first, ok := PartialEligible(old, twoReachDef(0.020)); !ok || first != 1 {
		t.Errorf("upstream n change: first=%d ok=%v, want 1,true", first, ok)
	}

	// Changed flow: no reuse.
	chgQ := twoReachDef(0.020)
	chgQ.DesignFlow = 6
	if _, ok := PartialEligible(old, chgQ); ok {
		t.Error("changed design flow must force full recompute")
	}

	// Changed control: no reuse.
	chgCtrl := twoReachDef(0.020)
	chgCtrl.Control.CrestHeight = 1.2
	if _, ok := PartialEligible(old, chgCtrl); ok {
		t.Error("changed control must force full recompute")
	}

	// Downstream reach changed: its upstream reach shares spec with the old
	// upstream reach, but the reusable suffix is empty -> no reuse.
	chgDown := twoReachDef(0.020)
	chgDown.Reaches[1].Roughness = 0.018
	if _, ok := PartialEligible(old, chgDown); ok {
		t.Error("changed downstream reach must force full recompute")
	}
}

// TestPartialMatchesFull is the hard invariant: partial recomputation of an
// upstream-roughness change must reproduce the full recomputation point for
// point (within a tight tolerance), and must leave every suffix point
// bit-identical. This is case V at the engine level.
func TestPartialMatchesFull(t *testing.T) {
	old := twoReachDef(0.015)
	new := twoReachDef(0.020)

	oldProf, err := Compute(old, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	fullProf, err := Compute(new, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	first, ok := PartialEligible(old, new)
	if !ok || first != 1 {
		t.Fatalf("partial eligibility: first=%d ok=%v", first, ok)
	}
	partProf, err := RecomputePartial(old, new, first, oldProf, Hooks{})
	if err != nil {
		t.Fatal(err)
	}

	// Same station set and depths within tolerance everywhere.
	const tol = 1e-9
	if len(partProf.Points) != len(fullProf.Points) {
		t.Fatalf("point count partial %d != full %d", len(partProf.Points), len(fullProf.Points))
	}
	var maxErr float64
	for i := range fullProf.Points {
		a, b := partProf.Points[i], fullProf.Points[i]
		if math.Abs(a.Distance-b.Distance) > 1e-9 {
			t.Fatalf("station %d distances differ: %.6f vs %.6f", i, a.Distance, b.Distance)
		}
		if e := math.Abs(a.Depth - b.Depth); e > maxErr {
			maxErr = e
		}
		if a.Reach != b.Reach {
			t.Fatalf("station %.0f reach labels differ: %d vs %d", a.Distance, a.Reach, b.Reach)
		}
	}
	if maxErr > tol {
		t.Errorf("partial vs full max depth difference %.2e > %.0e", maxErr, tol)
	}

	// Suffix points copied verbatim from the old profile (identical floats).
	boundaryS := new.ReachDownDistance(first)
	for _, pt := range partProf.Points {
		if pt.Distance < boundaryS-1e-9 {
			oy := DepthAt(oldProf, pt.Distance)
			if pt.Depth != oy {
				t.Fatalf("suffix depth at %.0f changed by partial recompute: %.12f != %.12f",
					pt.Distance, pt.Depth, oy)
			}
		}
	}

	// Roughness up in an M1 region means depths there cannot decrease: the
	// comparison summary should point upstream of the junction.
	comp := CompareProfiles(1, oldProf, fullProf)
	if comp.MaxDeltaDistance < boundaryS {
		t.Errorf("max delta at s=%.1f, expected within changed prefix (>= %.1f)",
			comp.MaxDeltaDistance, boundaryS)
	}
	if comp.MaxDeltaDepth < 0 {
		t.Errorf("rougher upstream reach should raise depths, delta %.2e", comp.MaxDeltaDepth)
	}
}

// TestPartialWithInsertedReach verifies index relabelling when a reach is
// inserted upstream of an unchanged two-reach line.
func TestPartialWithInsertedReach(t *testing.T) {
	old := twoReachDef(0.015)
	inserted := channel.Reach{
		Length:    1000,
		Section:   geometry.Section{BottomWidth: 3, SideSlope: 0.25},
		Roughness: 0.022,
		Slope:     0.0009,
	}
	new := channel.Definition{
		Reaches:    append([]channel.Reach{inserted}, old.Reaches...),
		DesignFlow: old.DesignFlow,
		Control:    old.Control,
	}

	oldProf, err := Compute(old, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	fullProf, err := Compute(new, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	first, ok := PartialEligible(old, new)
	if !ok || first != 1 {
		t.Fatalf("partial eligibility: first=%d ok=%v", first, ok)
	}
	partProf, err := RecomputePartial(old, new, first, oldProf, Hooks{})
	if err != nil {
		t.Fatal(err)
	}

	if len(partProf.Reaches) != 3 {
		t.Fatalf("reach summaries = %d, want 3", len(partProf.Reaches))
	}
	// Every copied suffix point must be labelled with the NEW indices.
	for _, pt := range partProf.Points {
		want := reachAtDistance(new, pt.Distance)
		if pt.Reach != want {
			t.Errorf("point s=%.0f reach = %d, want %d", pt.Distance, pt.Reach, want)
		}
	}
	// Suffix reach summary relabelled and identical numerically.
	if math.Abs(partProf.Reaches[2].DepthDown-fullProf.Reaches[2].DepthDown) > 1e-9 {
		t.Errorf("relabelled suffix depth %.12f != %.12f",
			partProf.Reaches[2].DepthDown, fullProf.Reaches[2].DepthDown)
	}
	// Prefix agrees with full.
	var maxErr float64
	for _, b := range fullProf.Points {
		if a := DepthAt(partProf, b.Distance); math.Abs(a-b.Depth) > maxErr {
			maxErr = math.Abs(a - b.Depth)
		}
	}
	if maxErr > 1e-9 {
		t.Errorf("inserted-reach partial vs full max err %.2e", maxErr)
	}
}

// TestDepthAtInterpolation checks the sampler used by partial starts.
func TestDepthAtInterpolation(t *testing.T) {
	p := &Profile{Points: []Point{
		{Distance: 0, Depth: 2},
		{Distance: 100, Depth: 1},
	}}
	if got := DepthAt(p, 50); math.Abs(got-1.5) > 1e-12 {
		t.Errorf("DepthAt(50) = %v, want 1.5", got)
	}
	if DepthAt(p, -5) != 2 || DepthAt(p, 999) != 1 {
		t.Error("DepthAt clipping wrong")
	}
}
