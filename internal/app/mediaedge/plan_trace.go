package mediaedge

import (
	"fmt"
	"strings"
)

// planTrace renders what the shared encoder is doing (how many encoder sessions
// are active, at what rate) and each edge's demand, so extra sessions started for
// a weak viewer are visible in a debug log. It reports only changes.
type planTrace struct {
	last string
}

func (trace *planTrace) describe(plan OutputPlan, demands []outputDemand) (string, bool) {
	var slots []string
	active := 0
	for slot, on := range plan.Active {
		if on {
			active++
			slots = append(slots, fmt.Sprintf("%d:on@%dk", slot, plan.Bitrates[slot]/1000))
		} else {
			slots = append(slots, fmt.Sprintf("%d:off", slot))
		}
	}
	edges := []string{}
	for _, demand := range demands {
		if !demand.active {
			continue
		}
		kind := "full"
		if demand.lower {
			kind = "lower"
		}
		edges = append(edges, fmt.Sprintf("%s@%dk", kind, demand.budget/1000))
	}
	line := fmt.Sprintf("encoders=%d slots=[%s] edges=[%s]", active, strings.Join(slots, " "), strings.Join(edges, " "))
	changed := line != trace.last
	trace.last = line
	return line, changed
}
