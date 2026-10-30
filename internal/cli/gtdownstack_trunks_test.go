package cli

import (
	"slices"
	"testing"
)

func TestGTDownstackEndsAtAnyTrunkEntry(t *testing.T) {
	state := gtState{
		"dev":   {Trunk: true},
		"other": {Trunk: true},
		"b":     {Parents: []gtRef{{Ref: "a"}}},
		"a":     {Parents: []gtRef{{Ref: "dev"}}},
	}
	chain, err := gtDownstack("stack submit", state, "b", "other")
	if err != nil || !slices.Equal(chain, []string{"b", "a"}) {
		t.Fatalf("gtDownstack = %v, %v; want [b a] ending at the dev trunk entry", chain, err)
	}
}
