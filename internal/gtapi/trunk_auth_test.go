package gtapi

import (
	"context"
	"strings"
	"testing"
)

func TestTrunkSSMConfigurationSurvivesQueueSelectionOrder(t *testing.T) {
	t.Setenv("TRUNK_MERGE_API_KEY", "")
	config := TrunkSSMConfig{SSMParameter: "/ccx/queue"}
	for _, original := range []*Client{New("http://unused"), NewNative()} {
		for _, selected := range []*Client{
			original.WithTrunkSSM(config).WithQueueProvider("trunk"),
			original.WithQueueProvider("trunk").WithTrunkSSM(config),
			original.WithQueueProvider("trunk").WithTrunkSSM(config).WithQueueProvider("none").WithQueueProvider("trunk"),
		} {
			_, err := selected.MergeabilityStatuses(context.Background(), "org", "repo", []int{1})
			if err == nil || !strings.Contains(err.Error(), "requires a valid aws_profile") {
				t.Fatalf("configuration error was lost before credential lookup: %v", err)
			}
		}
		if original.trunkSSM != (TrunkSSMConfig{}) || original.queue != nil {
			t.Fatal("Trunk credential configuration mutated the original client")
		}
	}
}
