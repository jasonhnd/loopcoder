package orchestration

import (
	"fmt"
	"sort"

	compiler "github.com/jasonhnd/loopcoder/internal/compile"
	"github.com/jasonhnd/loopcoder/internal/report"
)

type epicOrderingHint struct {
	EpicID         string
	EpicTitle      string
	SliceID        string
	SliceRef       string
	Rank           int
	UnblockCount   int
	OnCriticalPath bool
	DependsOn      []string
}

func loadEpicOrderingHints(repoPath string) ([]report.EpicOrderingSummary, map[int]epicOrderingHint, map[int]string, error) {
	files, loadErrors, err := compiler.LoadEpicSliceDAGArtifactsBestEffort(repoPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load epic DAG ordering: %w", err)
	}
	summaries := make([]report.EpicOrderingSummary, 0, len(files))
	hints := map[int]epicOrderingHint{}
	needsHuman := map[int]string{}
	for _, loadErr := range loadErrors {
		reason := fmt.Sprintf("epic DAG artifact %s is unreadable or invalid: %v", loadErr.Path, loadErr.Err)
		for _, node := range loadErr.Artifact.Nodes {
			if node.Issue <= 0 {
				continue
			}
			needsHuman[node.Issue] = reason
		}
	}
	for _, file := range files {
		artifact := file.Artifact
		if artifact.Ordering == nil {
			continue
		}
		summary := report.EpicOrderingSummary{
			EpicID:          artifact.EpicID,
			EpicTitle:       artifact.EpicTitle,
			ArtifactPath:    file.Path,
			Ready:           orderNodeRefsForReport(artifact.Ordering.Ready),
			CriticalPath:    append([]string(nil), artifact.Ordering.CriticalPath...),
			CriticalPathETA: artifact.Ordering.CriticalPathETA,
			AtomicSlices:    atomicSlicesForReport(artifact.Ordering.AtomicSlices),
		}
		summaries = append(summaries, summary)
		for layerIndex, layer := range artifact.Ordering.Layers {
			for nodeIndex, node := range layer.Nodes {
				if node.Issue <= 0 {
					continue
				}
				hints[node.Issue] = epicOrderingHint{
					EpicID:         artifact.EpicID,
					EpicTitle:      artifact.EpicTitle,
					SliceID:        node.ID,
					SliceRef:       firstNonEmpty(node.Ref, node.ID),
					Rank:           layerIndex*100000 + nodeIndex,
					UnblockCount:   node.UnblockCount,
					OnCriticalPath: node.OnCriticalPath,
					DependsOn:      append([]string(nil), node.DependsOn...),
				}
			}
		}
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].EpicTitle == summaries[j].EpicTitle {
			return summaries[i].EpicID < summaries[j].EpicID
		}
		return summaries[i].EpicTitle < summaries[j].EpicTitle
	})
	return summaries, hints, needsHuman, nil
}

func applyEpicOrderingHints(ready []report.ReadyIssue, hints map[int]epicOrderingHint) {
	for i := range ready {
		hint, ok := hints[ready[i].Issue]
		if !ok {
			continue
		}
		ready[i].EpicID = hint.EpicID
		ready[i].EpicTitle = hint.EpicTitle
		ready[i].SliceID = hint.SliceID
		ready[i].SliceRef = hint.SliceRef
		ready[i].UnblockCount = hint.UnblockCount
		ready[i].OnCriticalPath = hint.OnCriticalPath
		ready[i].DependsOn = append([]string(nil), hint.DependsOn...)
	}
	sort.Slice(ready, func(i, j int) bool {
		left, leftOK := hints[ready[i].Issue]
		right, rightOK := hints[ready[j].Issue]
		if leftOK != rightOK {
			return leftOK
		}
		if leftOK && rightOK {
			if left.UnblockCount != right.UnblockCount {
				return left.UnblockCount > right.UnblockCount
			}
			if left.OnCriticalPath != right.OnCriticalPath {
				return left.OnCriticalPath
			}
			if len(left.DependsOn) != len(right.DependsOn) {
				return len(left.DependsOn) > len(right.DependsOn)
			}
			if left.Rank != right.Rank {
				return left.Rank < right.Rank
			}
		}
		return ready[i].Issue < ready[j].Issue
	})
}

func isolateBadEpicIssues(ready []report.ReadyIssue, blocked []report.BlockedIssue, needsHuman map[int]string) ([]report.ReadyIssue, []report.BlockedIssue) {
	if len(needsHuman) == 0 {
		return ready, blocked
	}
	filtered := make([]report.ReadyIssue, 0, len(ready))
	for _, item := range ready {
		reason, ok := needsHuman[item.Issue]
		if !ok {
			filtered = append(filtered, item)
			continue
		}
		blocked = append(blocked, report.BlockedIssue{
			Issue:          item.Issue,
			Title:          item.Title,
			Classification: "needs-human",
			Reason:         reason,
			Dependencies:   []int{},
			OpenPRs:        []report.OpenPRSummary{},
			Attempts:       []report.AttemptSummary{},
		})
	}
	sort.Slice(blocked, func(i, j int) bool {
		return blocked[i].Issue < blocked[j].Issue
	})
	return filtered, blocked
}

func orderNodeRefsForReport(nodes []compiler.EpicDAGOrderNode) []string {
	if nodes == nil {
		return []string{}
	}
	out := make([]string, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, firstNonEmpty(node.Ref, node.ID))
	}
	return out
}

func atomicSlicesForReport(slices []compiler.EpicAtomicSlice) []report.AtomicSliceSummary {
	if slices == nil {
		return []report.AtomicSliceSummary{}
	}
	out := make([]report.AtomicSliceSummary, 0, len(slices))
	for _, slice := range slices {
		members := make([]string, 0, len(slice.Members))
		for _, member := range slice.Members {
			members = append(members, firstNonEmpty(member.Ref, member.ID))
		}
		out = append(out, report.AtomicSliceSummary{
			Ref:     firstNonEmpty(slice.Ref, slice.ID),
			Issue:   slice.Issue,
			Members: members,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Ref < out[j].Ref
	})
	return out
}
