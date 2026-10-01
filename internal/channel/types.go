// Package channel holds the long-lived canal-line domain model: an ordered
// sequence of prismatic reaches, a design discharge and a downstream control
// (sharp-crested weir or prescribed depth).
//
// A channel is immutable once created; every modification appends a new
// version. Reaches are stored in upstream-to-downstream order, i.e.
// reaches[0] is the most upstream reach and the last reach ends at the
// downstream control.
package channel

import "openchannel/internal/geometry"

// Reach is one prismatic segment of the canal line.
type Reach struct {
	Name      string           `json:"name,omitempty"` // optional human label
	Length    float64          `json:"length"`         // metres, > 0
	Section   geometry.Section `json:"section"`        // trapezoid (m > 0) or rectangle (m == 0)
	Roughness float64          `json:"roughness"`      // Manning's n (SI), > 0
	Slope     float64          `json:"slope"`          // channel-bottom slope S0, > 0
}

// ControlKind discriminates the downstream boundary condition.
type ControlKind string

const (
	// ControlWeir fixes the downstream stage through a sharp-crested weir.
	ControlWeir ControlKind = "weir"
	// ControlDepth fixes the downstream water depth directly.
	ControlDepth ControlKind = "depth"
)

// Control is the downstream boundary condition. Exactly one variant is set:
// for Kind == ControlWeir the weir fields are used; for Kind == ControlDepth
// DepthInMetres is used.
type Control struct {
	Kind        ControlKind `json:"kind"`
	Width       float64     `json:"width,omitempty"`        // weir crest width b, metres
	CrestHeight float64     `json:"crest_height,omitempty"` // weir crest height p above the last reach bed, metres (>= 0)
	Cd          float64     `json:"discharge_coefficient,omitempty"`
	Depth       float64     `json:"depth,omitempty"` // prescribed downstream water depth, metres
}

// Definition is the full, immutable content of one channel version.
type Definition struct {
	Reaches    []Reach `json:"reaches"` // upstream first, downstream last; non-empty
	DesignFlow float64 `json:"design_flow"`
	Control    Control `json:"control"`
}

// TotalLength sums the reach lengths (in metres).
func (d Definition) TotalLength() float64 {
	var sum float64
	for _, r := range d.Reaches {
		sum += r.Length
	}
	return sum
}

// ReachDownDistance returns the distance from the downstream control to the
// DOWNSTREAM end of reach i.
func (d Definition) ReachDownDistance(i int) float64 {
	var cum float64
	for j := i + 1; j < len(d.Reaches); j++ {
		cum += d.Reaches[j].Length
	}
	return cum
}

// ReachUpDistance returns the distance from the downstream control to the
// UPSTREAM end of reach i.
func (d Definition) ReachUpDistance(i int) float64 {
	var cum float64
	for j := i; j < len(d.Reaches); j++ {
		cum += d.Reaches[j].Length
	}
	return cum
}

// Metadata is the identity/history part of a stored version, without the
// potentially large definition body.
type Metadata struct {
	ChannelID string `json:"channel_id"`
	Version   int    `json:"version"`
	CreatedAt string `json:"created_at"` // RFC3339, UTC
}
