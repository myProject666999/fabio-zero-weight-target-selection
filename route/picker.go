package route

import (
	"math/rand"
	"sync/atomic"
)

// picker selects a target from a list of targets.
type picker func(r *Route) *Target

// Picker contains the available picker functions.
// Update config/load.go#load after updating.
var Picker = map[string]picker{
	"rnd": rndPicker,
	"rr":  rrPicker,
}

// rndPicker picks a random target from the list of targets.
// It returns nil if the route has no targets which can receive traffic.
func rndPicker(r *Route) *Target {
	if len(r.wTargets) == 0 {
		return nil
	}
	return r.wTargets[randIntn(len(r.wTargets))]
}

// rrPicker picks the next target from a list of targets using round-robin.
// It returns nil if the route has no targets which can receive traffic.
func rrPicker(r *Route) *Target {
	if len(r.wTargets) == 0 {
		return nil
	}
	n := atomic.AddUint64(&r.total, 1)
	return r.wTargets[(n-1)%uint64(len(r.wTargets))]
}

// as it turns out, math/rand's Intn is now way faster (4x) than the previous implementation using
// time.UnixNano().  As a bonus, this actually works properly on 32 bit platforms.
var randIntn = func(n int) int {
	if n == 0 {
		return 0
	}
	return rand.Intn(n)
}
