package route

import (
	"encoding/json"
	"math"
)

type Cmd string

const (
	RouteAddCmd    Cmd = "route add"
	RouteDelCmd    Cmd = "route del"
	RouteWeightCmd Cmd = "route weight"
)

type RouteDef struct {
	Opts    map[string]string `json:"opts,omitempty"`
	Cmd     Cmd               `json:"cmd"`
	Service string            `json:"service"`
	Src     string            `json:"src"`
	Dst     string            `json:"dst"`
	Tags    []string          `json:"tags,omitempty"`
	Weight  float64           `json:"weight"`
}

// UnmarshalJSON decodes a RouteDef from the JSON used by the custom backend
// and distinguishes an omitted weight field from an explicit "weight": 0.
//
// An omitted weight means that the target has no fixed weight and participates
// in the dynamic (even) distribution of the remaining traffic. This keeps
// custom backends which do not send a weight field working as before.
//
// An explicit "weight": 0 drains the target, i.e. it receives no traffic.
//
// For "route weight" commands an omitted weight is not a valid command since
// it cannot be distinguished from accidentally draining all matching targets.
// Such a definition is marked invalid and NewTableCustom logs and ignores it.
// It must not be interpreted as weight 0.
//
// Marshalling is not overridden, so the serialized shape of RouteDef is
// unchanged and always contains the "weight" field.
func (d *RouteDef) UnmarshalJSON(b []byte) error {
	type routeDef RouteDef
	var aux struct {
		routeDef
		Weight *float64 `json:"weight"`
	}
	aux.routeDef = routeDef(*d)

	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	*d = RouteDef(aux.routeDef)

	// The embedded struct does not see the "weight" field of the outer struct.
	// An explicit "weight": null or a missing weight therefore leaves Weight
	// unset. An explicit number is taken from aux.Weight.
	d.Weight = 0
	switch {
	case aux.Weight != nil:
		d.Weight = *aux.Weight
	case d.Cmd == RouteWeightCmd:
		// sentinel for "weight field missing in a route weight command"
		d.Weight = math.NaN()
	default:
		d.Weight = -1
	}
	return nil
}
