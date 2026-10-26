package mediaedge

import "testing"

func TestPlanTraceReportsEncodersAndEdgeDemandOnlyWhenTheyChange(t *testing.T) {
	var trace planTrace
	plan := OutputPlan{Active: []bool{true, true, false}, Bitrates: []uint32{4_000_000, 12_000_000, 0}}
	demands := []outputDemand{
		{active: true, full: 12_000_000, budget: 12_000_000},
		{active: true, lower: true, full: 3_000_000, budget: 3_000_000},
	}
	line, changed := trace.describe(plan, demands)
	if !changed {
		t.Fatal("the first plan must be reported")
	}
	want := "encoders=2 slots=[0:on@4000k 1:on@12000k 2:off] edges=[full@12000k lower@3000k]"
	if line != want {
		t.Fatalf("got  %q\nwant %q", line, want)
	}
	if _, changed := trace.describe(plan, demands); changed {
		t.Fatal("an identical plan was reported again")
	}
	plan.Active[2], plan.Bitrates[2] = true, 3_000_000
	line, changed = trace.describe(plan, demands)
	if !changed || line == want {
		t.Fatalf("a third encoder starting was not reported: %q", line)
	}
}

func TestPlanTraceCountsOnlyActiveEncoders(t *testing.T) {
	var trace planTrace
	line, _ := trace.describe(OutputPlan{Active: []bool{false, false}, Bitrates: []uint32{0, 0}}, nil)
	if line != "encoders=0 slots=[0:off 1:off] edges=[]" {
		t.Fatalf("got %q", line)
	}
}
