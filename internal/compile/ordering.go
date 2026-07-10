package compile

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/pathid"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
)

const goListHardCapDefault = lcdefaults.CompileGoListHardCap

var (
	goListCommand = "go"
	goListArgs    = []string{"list", "-json", "./..."}
	goListHardCap = goListHardCapDefault
)

func ExtractGoListBackbone(ctx context.Context, repoPath string) (GoListBackbone, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	backbone := unavailableGoListBackbone()
	if _, err := os.Stat(filepath.Join(repoPath, "go.mod")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return backbone, nil
		}
		return GoListBackbone{}, fmt.Errorf("inspect go.mod for go list backbone: %w", err)
	}

	cmd := exec.CommandContext(ctx, goListCommand, goListArgs...)
	cmd.Dir = repoPath
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	result, err := supervisedexec.Run(ctx, cmd, supervisedexec.Options{HardCap: goListHardCap})
	if err != nil || result.Outcome != supervisedexec.OutcomeCompleted || result.ExitCode != 0 {
		return backbone, nil
	}
	parsed, err := parseGoListBackbone(repoPath, stdout.Bytes())
	if err != nil {
		return backbone, nil
	}
	if parsed.Available && len(parsed.Packages) == 0 && repoHasGoSource(repoPath) {
		return backbone, nil
	}
	return parsed, nil
}

func unavailableGoListBackbone() GoListBackbone {
	return GoListBackbone{
		Tool:      "go list",
		Pattern:   "./...",
		Available: false,
		Packages:  []GoListPackage{},
		Edges:     []GoListEdge{},
	}
}

type goListPackageJSON struct {
	ImportPath string   `json:"ImportPath"`
	Dir        string   `json:"Dir"`
	Name       string   `json:"Name"`
	Imports    []string `json:"Imports"`
}

func parseGoListBackbone(repoPath string, data []byte) (GoListBackbone, error) {
	repoIdentity, err := pathid.Identity(repoPath)
	if err != nil {
		return GoListBackbone{}, fmt.Errorf("resolve repo path for go list backbone: %w", err)
	}

	var raw []goListPackageJSON
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var pkg goListPackageJSON
		if err := decoder.Decode(&pkg); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return GoListBackbone{}, fmt.Errorf("parse go list backbone: %w", err)
		}
		if strings.TrimSpace(pkg.ImportPath) != "" {
			raw = append(raw, pkg)
		}
	}

	local := map[string]goListPackageJSON{}
	packages := make([]GoListPackage, 0, len(raw))
	for _, pkg := range raw {
		rel, ok := repoRelativeDir(repoIdentity, pkg.Dir)
		if !ok {
			continue
		}
		local[pkg.ImportPath] = pkg
		packages = append(packages, GoListPackage{
			ImportPath: pkg.ImportPath,
			Dir:        rel,
			Name:       pkg.Name,
		})
	}
	sort.Slice(packages, func(i, j int) bool {
		return packages[i].ImportPath < packages[j].ImportPath
	})

	edgeSeen := map[string]bool{}
	var edges []GoListEdge
	for _, pkg := range raw {
		if _, ok := local[pkg.ImportPath]; !ok {
			continue
		}
		for _, imported := range pkg.Imports {
			if _, ok := local[imported]; !ok {
				continue
			}
			key := pkg.ImportPath + "\x00" + imported
			if edgeSeen[key] {
				continue
			}
			edgeSeen[key] = true
			edges = append(edges, GoListEdge{From: imported, To: pkg.ImportPath})
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From == edges[j].From {
			return edges[i].To < edges[j].To
		}
		return edges[i].From < edges[j].From
	})
	if len(raw) > 0 && len(packages) == 0 {
		return GoListBackbone{}, fmt.Errorf("go list returned %d package(s), but none resolved under repository %s", len(raw), repoIdentity)
	}

	return GoListBackbone{
		Tool:      "go list",
		Pattern:   "./...",
		Available: true,
		Packages:  packages,
		Edges:     edges,
	}, nil
}

func repoRelativeDir(repoIdentity, dir string) (string, bool) {
	if strings.TrimSpace(dir) == "" {
		return "", false
	}
	dirIdentity, err := pathid.Identity(dir)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(repoIdentity, dirIdentity)
	if err != nil {
		return "", false
	}
	if rel == "." {
		return ".", true
	}
	if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func repoHasGoSource(repoPath string) bool {
	found := false
	_ = filepath.WalkDir(repoPath, func(path string, entry os.DirEntry, err error) error {
		if err != nil || found {
			return nil
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".loopcoder", "vendor":
				if path != repoPath {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if strings.HasSuffix(entry.Name(), ".go") && !strings.HasSuffix(entry.Name(), "_test.go") {
			found = true
		}
		return nil
	})
	return found
}

type EpicDAGArtifactFile struct {
	Path     string
	Artifact EpicSliceDAGArtifact
}

type EpicDAGArtifactLoadError struct {
	Path     string
	Artifact EpicSliceDAGArtifact
	Err      error
}

func (e EpicDAGArtifactLoadError) Error() string {
	return fmt.Sprintf("%s: %v", e.Path, e.Err)
}

func (e EpicDAGArtifactLoadError) Unwrap() error {
	return e.Err
}

func LoadEpicSliceDAGArtifacts(repoPath string) ([]EpicDAGArtifactFile, error) {
	artifacts, loadErrors, err := LoadEpicSliceDAGArtifactsBestEffort(repoPath)
	if err != nil {
		return nil, err
	}
	if len(loadErrors) > 0 {
		return nil, loadErrors[0]
	}
	return artifacts, nil
}

func LoadEpicSliceDAGArtifactsBestEffort(repoPath string) ([]EpicDAGArtifactFile, []EpicDAGArtifactLoadError, error) {
	root := filepath.Join(repoPath, epicDAGRelDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("read epic DAG artifacts: %w", err)
	}

	var artifacts []EpicDAGArtifactFile
	var loadErrors []EpicDAGArtifactLoadError
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".slice_dag.json") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		artifact, err := loadEpicSliceDAGArtifact(path)
		if err != nil {
			loadErrors = append(loadErrors, EpicDAGArtifactLoadError{
				Path:     filepath.ToSlash(path),
				Artifact: artifact,
				Err:      err,
			})
			continue
		}
		artifacts = append(artifacts, EpicDAGArtifactFile{
			Path:     filepath.ToSlash(path),
			Artifact: artifact,
		})
	}
	sort.Slice(artifacts, func(i, j int) bool {
		return artifacts[i].Path < artifacts[j].Path
	})
	sort.Slice(loadErrors, func(i, j int) bool {
		return loadErrors[i].Path < loadErrors[j].Path
	})
	return artifacts, loadErrors, nil
}

func loadEpicSliceDAGArtifact(path string) (EpicSliceDAGArtifact, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return EpicSliceDAGArtifact{}, fmt.Errorf("read epic DAG artifact: %w", err)
	}
	var artifact EpicSliceDAGArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		return artifact, fmt.Errorf("parse epic DAG artifact: %w", err)
	}
	if artifact.Ordering == nil {
		ordering, err := computeEpicOrdering(artifact.Nodes)
		if err != nil {
			return artifact, fmt.Errorf("order epic DAG artifact: %w", err)
		}
		artifact.Ordering = &ordering
	}
	return artifact, nil
}

func condensePlanItemCycles(items []*planItem) []*planItem {
	components := planItemSCCs(items)
	cyclic := false
	for _, component := range components {
		if len(component) > 1 {
			cyclic = true
			break
		}
	}
	if !cyclic {
		return items
	}

	type condensation struct {
		members []*planItem
		item    *planItem
		min     int
		atomic  bool
	}

	var groups []condensation
	atomicOrdinals := map[string]int{}
	for _, component := range components {
		sortItemsByOrder(component)
		first := component[0]
		group := condensation{members: component, min: first.order, atomic: len(component) > 1}
		if !group.atomic {
			group.item = first
			groups = append(groups, group)
			continue
		}
		prefix := sliceRefPrefix(first.ref)
		atomicOrdinals[prefix]++
		members := make([]EpicAtomicMember, 0, len(component))
		titles := make([]string, 0, len(component))
		kind := "doc"
		for _, member := range component {
			if member.kind == "code" {
				kind = "code"
			}
			members = append(members, EpicAtomicMember{
				ID:    member.id,
				Ref:   member.ref,
				Kind:  member.kind,
				Title: member.title,
			})
			titles = append(titles, member.title)
		}
		group.item = &planItem{
			id:            atomicPlanItemID(component),
			ref:           fmt.Sprintf("%s/atomic-%d", prefix, atomicOrdinals[prefix]),
			kind:          kind,
			title:         "Atomic slice: " + strings.Join(titles, " / "),
			unitTitle:     first.unitTitle,
			unitIntent:    first.unitIntent,
			sliceText:     atomicSliceText(members),
			order:         first.order,
			epicID:        first.epicID,
			epicRef:       first.epicRef,
			epicTitle:     first.epicTitle,
			atomic:        true,
			atomicMembers: members,
		}
		groups = append(groups, group)
	}
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].min < groups[j].min
	})

	idMap := map[string]string{}
	for _, group := range groups {
		for _, member := range group.members {
			idMap[member.id] = group.item.id
		}
	}
	out := make([]*planItem, 0, len(groups))
	for _, group := range groups {
		seen := map[string]bool{}
		var deps []string
		for _, member := range group.members {
			for _, depID := range member.depIDs {
				mapped := idMap[depID]
				if mapped == "" || mapped == group.item.id || seen[mapped] {
					continue
				}
				seen[mapped] = true
				deps = append(deps, mapped)
			}
		}
		sort.Strings(deps)
		group.item.depIDs = deps
		out = append(out, group.item)
	}
	sortItemsByOrder(out)
	return out
}

func planItemSCCs(items []*planItem) [][]*planItem {
	byID := map[string]*planItem{}
	for _, item := range items {
		byID[item.id] = item
	}

	index := 0
	indexes := map[string]int{}
	lowlinks := map[string]int{}
	onStack := map[string]bool{}
	var stack []*planItem
	var components [][]*planItem

	var strongConnect func(*planItem)
	strongConnect = func(item *planItem) {
		indexes[item.id] = index
		lowlinks[item.id] = index
		index++
		stack = append(stack, item)
		onStack[item.id] = true

		deps := append([]string(nil), item.depIDs...)
		sort.Slice(deps, func(i, j int) bool {
			left := byID[deps[i]]
			right := byID[deps[j]]
			if left != nil && right != nil && left.order != right.order {
				return left.order < right.order
			}
			return deps[i] < deps[j]
		})
		for _, depID := range deps {
			dep := byID[depID]
			if dep == nil {
				continue
			}
			if _, ok := indexes[dep.id]; !ok {
				strongConnect(dep)
				if lowlinks[dep.id] < lowlinks[item.id] {
					lowlinks[item.id] = lowlinks[dep.id]
				}
			} else if onStack[dep.id] && indexes[dep.id] < lowlinks[item.id] {
				lowlinks[item.id] = indexes[dep.id]
			}
		}

		if lowlinks[item.id] != indexes[item.id] {
			return
		}
		var component []*planItem
		for {
			last := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[last.id] = false
			component = append(component, last)
			if last.id == item.id {
				break
			}
		}
		sortItemsByOrder(component)
		components = append(components, component)
	}

	ordered := append([]*planItem(nil), items...)
	sortItemsByOrder(ordered)
	for _, item := range ordered {
		if _, ok := indexes[item.id]; !ok {
			strongConnect(item)
		}
	}
	sort.Slice(components, func(i, j int) bool {
		return components[i][0].order < components[j][0].order
	})
	return components
}

func atomicPlanItemID(items []*planItem) string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.id)
	}
	sort.Strings(ids)
	sum := sha1.Sum([]byte(strings.Join(ids, ",")))
	return "atomic-" + hex.EncodeToString(sum[:])[:12]
}

func atomicSliceText(members []EpicAtomicMember) string {
	var out strings.Builder
	out.WriteString("These slices form a dependency cycle and must ship atomically in one PR.")
	for _, member := range members {
		out.WriteString("\n- ")
		out.WriteString(member.Ref)
		out.WriteString(" ")
		out.WriteString(member.Kind)
		out.WriteString(": ")
		out.WriteString(member.Title)
	}
	return out.String()
}

func sliceRefPrefix(ref string) string {
	if before, _, ok := strings.Cut(ref, "/"); ok && before != "" {
		return before
	}
	return "epic"
}

func computeEpicOrdering(nodes []EpicSliceNode) (EpicDAGOrdering, error) {
	nodeByID := map[string]EpicSliceNode{}
	indexByID := map[string]int{}
	for i, node := range nodes {
		if strings.TrimSpace(node.ID) == "" {
			continue
		}
		node.DependsOn = sortedStrings(node.DependsOn)
		nodeByID[node.ID] = node
		indexByID[node.ID] = i
	}

	remaining := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node.ID == "" || node.Completed {
			continue
		}
		remaining = append(remaining, node.ID)
	}
	sort.Slice(remaining, func(i, j int) bool {
		return nodeLess(nodeByID, indexByID, remaining[i], remaining[j])
	})

	dependents := map[string][]string{}
	for _, node := range nodes {
		if node.ID == "" || node.Completed {
			continue
		}
		for _, depID := range node.DependsOn {
			dep, ok := nodeByID[depID]
			if !ok || dep.Completed {
				continue
			}
			dependents[depID] = append(dependents[depID], node.ID)
		}
	}
	for id := range dependents {
		sort.Slice(dependents[id], func(i, j int) bool {
			return nodeLess(nodeByID, indexByID, dependents[id][i], dependents[id][j])
		})
	}
	if hasEpicOrderingCycle(remaining, dependents) {
		return EpicDAGOrdering{}, errors.New("epic slice DAG contains a cycle after SCC condensation")
	}

	criticalPath := epicCriticalPath(remaining, dependents, nodeByID, indexByID)
	onCritical := map[string]bool{}
	for _, ref := range criticalPath {
		for _, node := range nodes {
			if firstNonEmpty(node.Ref, node.ID) == ref {
				onCritical[node.ID] = true
				break
			}
		}
	}
	unblockCounts := map[string]int{}
	for _, id := range remaining {
		unblockCounts[id] = len(dependents[id])
	}

	orderNode := func(id string) EpicDAGOrderNode {
		node := nodeByID[id]
		return EpicDAGOrderNode{
			ID:             node.ID,
			Ref:            firstNonEmpty(node.Ref, node.ID),
			Issue:          node.Issue,
			UnblockCount:   unblockCounts[id],
			OnCriticalPath: onCritical[id],
			DependsOn:      sortedStrings(node.DependsOn),
		}
	}
	sortReadyIDs := func(ids []string) {
		sort.Slice(ids, func(i, j int) bool {
			return readyNodeLess(ids[i], ids[j], nodeByID, indexByID, unblockCounts, onCritical)
		})
	}

	remainingDeps := map[string]int{}
	for _, id := range remaining {
		node := nodeByID[id]
		for _, depID := range node.DependsOn {
			dep, ok := nodeByID[depID]
			if ok && !dep.Completed {
				remainingDeps[id]++
			}
		}
	}
	var ready []string
	for _, id := range remaining {
		if remainingDeps[id] == 0 {
			ready = append(ready, id)
		}
	}
	sortReadyIDs(ready)

	var layers []EpicDAGLayer
	seen := 0
	for len(ready) > 0 {
		current := append([]string(nil), ready...)
		layer := EpicDAGLayer{Index: len(layers), Nodes: make([]EpicDAGOrderNode, 0, len(current))}
		for _, id := range current {
			layer.Nodes = append(layer.Nodes, orderNode(id))
		}
		layers = append(layers, layer)
		seen += len(current)

		var next []string
		for _, id := range current {
			for _, dependent := range dependents[id] {
				remainingDeps[dependent]--
				if remainingDeps[dependent] == 0 {
					next = append(next, dependent)
				}
			}
		}
		sortReadyIDs(next)
		ready = next
	}
	if seen != len(remaining) {
		return EpicDAGOrdering{}, errors.New("epic slice DAG contains a cycle after SCC condensation")
	}

	ordering := EpicDAGOrdering{
		Ready:           []EpicDAGOrderNode{},
		Layers:          layers,
		CriticalPath:    normalizeStrings(criticalPath),
		CriticalPathETA: len(criticalPath),
		AtomicSlices:    atomicSlicesFromNodes(nodes),
	}
	if len(layers) > 0 {
		ordering.Ready = layers[0].Nodes
	}
	return ordering, nil
}

func hasEpicOrderingCycle(remaining []string, dependents map[string][]string) bool {
	state := map[string]int{}
	var visit func(string) bool
	visit = func(id string) bool {
		switch state[id] {
		case 1:
			return true
		case 2:
			return false
		}
		state[id] = 1
		for _, dependent := range dependents[id] {
			if visit(dependent) {
				return true
			}
		}
		state[id] = 2
		return false
	}
	for _, id := range remaining {
		if visit(id) {
			return true
		}
	}
	return false
}

func epicCriticalPath(remaining []string, dependents map[string][]string, nodeByID map[string]EpicSliceNode, indexByID map[string]int) []string {
	memo := map[string][]string{}
	var walk func(string) []string
	walk = func(id string) []string {
		if path, ok := memo[id]; ok {
			return path
		}
		best := []string{id}
		for _, dependent := range dependents[id] {
			child := walk(dependent)
			candidate := append([]string{id}, child...)
			if len(candidate) > len(best) || (len(candidate) == len(best) && pathLess(candidate, best, nodeByID, indexByID)) {
				best = candidate
			}
		}
		memo[id] = best
		return best
	}

	var best []string
	for _, id := range remaining {
		path := walk(id)
		if len(path) > len(best) || (len(path) == len(best) && pathLess(path, best, nodeByID, indexByID)) {
			best = path
		}
	}
	refs := make([]string, 0, len(best))
	for _, id := range best {
		node := nodeByID[id]
		refs = append(refs, firstNonEmpty(node.Ref, node.ID))
	}
	return refs
}

func pathLess(left, right []string, nodeByID map[string]EpicSliceNode, indexByID map[string]int) bool {
	if len(right) == 0 {
		return true
	}
	for i := 0; i < len(left) && i < len(right); i++ {
		if left[i] == right[i] {
			continue
		}
		return nodeLess(nodeByID, indexByID, left[i], right[i])
	}
	return len(left) < len(right)
}

func readyNodeLess(left, right string, nodeByID map[string]EpicSliceNode, indexByID map[string]int, unblockCounts map[string]int, onCritical map[string]bool) bool {
	if unblockCounts[left] != unblockCounts[right] {
		return unblockCounts[left] > unblockCounts[right]
	}
	if onCritical[left] != onCritical[right] {
		return onCritical[left]
	}
	if len(nodeByID[left].DependsOn) != len(nodeByID[right].DependsOn) {
		return len(nodeByID[left].DependsOn) > len(nodeByID[right].DependsOn)
	}
	return nodeLess(nodeByID, indexByID, left, right)
}

func nodeLess(nodeByID map[string]EpicSliceNode, indexByID map[string]int, left, right string) bool {
	leftNode := nodeByID[left]
	rightNode := nodeByID[right]
	leftKey := firstNonEmpty(leftNode.Ref, leftNode.ID)
	rightKey := firstNonEmpty(rightNode.Ref, rightNode.ID)
	if leftKey != rightKey {
		return leftKey < rightKey
	}
	if indexByID[left] != indexByID[right] {
		return indexByID[left] < indexByID[right]
	}
	return left < right
}

func atomicSlicesFromNodes(nodes []EpicSliceNode) []EpicAtomicSlice {
	var out []EpicAtomicSlice
	for _, node := range nodes {
		if !node.Atomic {
			continue
		}
		out = append(out, EpicAtomicSlice{
			ID:      node.ID,
			Ref:     firstNonEmpty(node.Ref, node.ID),
			Issue:   node.Issue,
			Members: append([]EpicAtomicMember(nil), node.AtomicMembers...),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Ref < out[j].Ref
	})
	return out
}

func orderNodeRefs(nodes []EpicDAGOrderNode) []string {
	if nodes == nil {
		return []string{}
	}
	out := make([]string, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, firstNonEmpty(node.Ref, node.ID))
	}
	return out
}

func atomicSliceRefs(slices []EpicAtomicSlice) []string {
	if slices == nil {
		return []string{}
	}
	out := make([]string, 0, len(slices))
	for _, slice := range slices {
		members := make([]string, 0, len(slice.Members))
		for _, member := range slice.Members {
			members = append(members, firstNonEmpty(member.Ref, member.ID))
		}
		out = append(out, fmt.Sprintf("%s (%s)", firstNonEmpty(slice.Ref, slice.ID), strings.Join(members, ", ")))
	}
	sort.Strings(out)
	return out
}

func formatStringList(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ", ")
}

func sortedAtomicMembers(values []EpicAtomicMember) []EpicAtomicMember {
	out := append([]EpicAtomicMember(nil), values...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ref != out[j].Ref {
			return out[i].Ref < out[j].Ref
		}
		return out[i].ID < out[j].ID
	})
	return out
}
