package orchestration

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

func childPlanFingerprint(plan ChildPlan) (string, string, error) {
	canonical, err := canonicalChildPlanJSON(plan)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(canonical))
	return fmt.Sprintf("sha256:%x", sum[:]), canonical, nil
}

func canonicalChildPlanJSON(plan ChildPlan) (string, error) {
	copyPlan := plan
	copyPlan.Items = cloneChildPlans(plan.Items)
	for i := range copyPlan.Items {
		copyPlan.Items[i].RunID = ""
		if len(strings.TrimSpace(string(copyPlan.Items[i].Metadata))) > 0 {
			normalized, err := canonicalRawJSON(copyPlan.Items[i].Metadata)
			if err != nil {
				return "", fmt.Errorf("canonicalize child %q metadata: %w", copyPlan.Items[i].ChildKey, err)
			}
			copyPlan.Items[i].Metadata = normalized
		}
	}
	data, err := json.Marshal(copyPlan)
	if err != nil {
		return "", fmt.Errorf("canonicalize child plan: %w", err)
	}
	return string(data), nil
}

func canonicalRawJSON(raw json.RawMessage) (json.RawMessage, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}

func childPlanMutationField(existingPlanJSON string, current ChildPlan) (string, error) {
	var existing ChildPlan
	if strings.TrimSpace(existingPlanJSON) == "" {
		return "plan_fingerprint", nil
	}
	existing = ChildPlan{}
	if err := json.Unmarshal([]byte(existingPlanJSON), &existing); err != nil {
		return "plan_json", nil
	}
	if err := ValidateChildPlan(&existing); err != nil {
		return "plan_json", nil
	}
	existingJSON, err := canonicalChildPlanJSON(existing)
	if err != nil {
		return "", err
	}
	currentJSON, err := canonicalChildPlanJSON(current)
	if err != nil {
		return "", err
	}
	var existingValue any
	var currentValue any
	if err := json.Unmarshal([]byte(existingJSON), &existingValue); err != nil {
		return "", err
	}
	if err := json.Unmarshal([]byte(currentJSON), &currentValue); err != nil {
		return "", err
	}
	return firstJSONDiffPath(existingValue, currentValue, "plan"), nil
}

func firstJSONDiffPath(left, right any, path string) string {
	if reflect.DeepEqual(left, right) {
		return ""
	}
	leftMap, leftOK := left.(map[string]any)
	rightMap, rightOK := right.(map[string]any)
	if leftOK && rightOK {
		keys := make([]string, 0, len(leftMap)+len(rightMap))
		seen := map[string]bool{}
		for key := range leftMap {
			keys = append(keys, key)
			seen[key] = true
		}
		for key := range rightMap {
			if !seen[key] {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			next := path + "." + key
			if diff := firstJSONDiffPath(leftMap[key], rightMap[key], next); diff != "" {
				return diff
			}
		}
		return path
	}
	leftSlice, leftOK := left.([]any)
	rightSlice, rightOK := right.([]any)
	if leftOK && rightOK {
		limit := len(leftSlice)
		if len(rightSlice) < limit {
			limit = len(rightSlice)
		}
		for i := 0; i < limit; i++ {
			if diff := firstJSONDiffPath(leftSlice[i], rightSlice[i], fmt.Sprintf("%s[%d]", path, i)); diff != "" {
				return diff
			}
		}
		return path
	}
	return path
}
