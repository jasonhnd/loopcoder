package orchestration

import (
	"context"
	"sort"
	"strings"

	compiler "github.com/jasonhnd/loopcoder/internal/compile"
)

type PromoteToggleInventoryFunc func(ctx context.Context, repoPath string) (PromoteToggleInventory, error)

type PromoteToggleInventory struct {
	FlipOn    []PromoteToggleItem `json:"flip_on"`
	LeaveDark []PromoteToggleItem `json:"leave_dark"`
}

type PromoteToggleItem struct {
	EpicID    string `json:"epic_id"`
	EpicTitle string `json:"epic_title"`
	SliceID   string `json:"slice_id"`
	SliceRef  string `json:"slice_ref"`
	BuildTag  string `json:"build_tag"`
	State     string `json:"state"`
	Action    string `json:"action"`
	Reason    string `json:"reason,omitempty"`
}

func BuildPromoteToggleInventory(_ context.Context, repoPath string) (PromoteToggleInventory, error) {
	files, err := compiler.LoadEpicSliceDAGArtifacts(repoPath)
	if err != nil {
		return PromoteToggleInventory{}, err
	}
	inventory := PromoteToggleInventory{
		FlipOn:    []PromoteToggleItem{},
		LeaveDark: []PromoteToggleItem{},
	}
	for _, file := range files {
		artifact := file.Artifact
		for _, node := range artifact.Nodes {
			if node.BuildTagToggle == nil {
				continue
			}
			item := PromoteToggleItem{
				EpicID:    artifact.EpicID,
				EpicTitle: artifact.EpicTitle,
				SliceID:   node.ID,
				SliceRef:  firstNonEmpty(node.Ref, node.ID),
				BuildTag:  node.BuildTagToggle.BuildTag,
				State:     node.BuildTagToggle.State,
			}
			if node.Dark || strings.EqualFold(node.BuildTagToggle.State, compiler.EpicToggleStateOff) {
				item.State = compiler.EpicToggleStateOff
				item.Action = "leave-dark"
				item.Reason = firstNonEmpty(node.DarkReason, "epic is not complete; leave build-tag toggle off")
				inventory.LeaveDark = append(inventory.LeaveDark, item)
				continue
			}
			item.State = compiler.EpicToggleStateOn
			item.Action = "flip-on"
			inventory.FlipOn = append(inventory.FlipOn, item)
		}
	}
	return normalizePromoteToggleInventory(inventory), nil
}

func normalizePromoteToggleInventory(inventory PromoteToggleInventory) PromoteToggleInventory {
	if inventory.FlipOn == nil {
		inventory.FlipOn = []PromoteToggleItem{}
	}
	if inventory.LeaveDark == nil {
		inventory.LeaveDark = []PromoteToggleItem{}
	}
	sortToggleItems(inventory.FlipOn)
	sortToggleItems(inventory.LeaveDark)
	return inventory
}

func sortToggleItems(items []PromoteToggleItem) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].EpicTitle != items[j].EpicTitle {
			return items[i].EpicTitle < items[j].EpicTitle
		}
		if items[i].SliceRef != items[j].SliceRef {
			return items[i].SliceRef < items[j].SliceRef
		}
		return items[i].SliceID < items[j].SliceID
	})
}
