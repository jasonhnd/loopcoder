// Package routecontract is the shared Gate-1/Gate-2A strict route requirement
// and child execution-contract identity package used by goalrun and workflowrun.
// No owner inference, no index invent, no silent defaults.
package routecontract

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/capclass"
)

// ParsedRouteRequirement is the exact structured routing contract for one child.
// All three fields are required and validated — never inferred from child index
// or owner labels.
type ParsedRouteRequirement struct {
	Class      capclass.Class
	Depth      string // canonical: low | medium | high
	Permission string // canonical exact: read-only | bounded_write
}

// ParseRouteRequirement requires exactly one valid class=, depth=, and permission=
// token. Rejects duplicate keys, empty comma tokens, empty values, unknown values,
// aliases, and needs_human (cannot spend).
func ParseRouteRequirement(routeReq string) (ParsedRouteRequirement, error) {
	raw := strings.TrimSpace(routeReq)
	if raw == "" {
		return ParsedRouteRequirement{}, fmt.Errorf("route_requirement empty")
	}
	seen := map[string]string{}
	// Split on commas without discarding empties — leading/trailing/double commas fail.
	for _, part := range strings.Split(raw, ",") {
		if strings.TrimSpace(part) == "" {
			return ParsedRouteRequirement{}, fmt.Errorf("route_requirement empty comma token (leading/trailing/double comma forbidden)")
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			return ParsedRouteRequirement{}, fmt.Errorf("route_requirement malformed token %q", part)
		}
		k := strings.ToLower(strings.TrimSpace(kv[0]))
		v := strings.TrimSpace(kv[1])
		if k == "" {
			return ParsedRouteRequirement{}, fmt.Errorf("route_requirement empty key in %q", part)
		}
		if v == "" {
			return ParsedRouteRequirement{}, fmt.Errorf("route_requirement empty value for key %q", k)
		}
		if _, dup := seen[k]; dup {
			return ParsedRouteRequirement{}, fmt.Errorf("route_requirement duplicate key %q", k)
		}
		seen[k] = v
	}
	for k := range seen {
		switch k {
		case "class", "depth", "permission":
		default:
			return ParsedRouteRequirement{}, fmt.Errorf("route_requirement unknown key %q", k)
		}
	}
	for _, req := range []string{"class", "depth", "permission"} {
		if _, ok := seen[req]; !ok {
			return ParsedRouteRequirement{}, fmt.Errorf("route_requirement missing %s= token", req)
		}
	}

	cl := capclass.Class(strings.ToLower(seen["class"]))
	if !cl.Valid() {
		return ParsedRouteRequirement{}, fmt.Errorf("route_requirement class=%q invalid", seen["class"])
	}
	if cl == capclass.ClassNeedsHuman {
		return ParsedRouteRequirement{}, fmt.Errorf("route_requirement class=needs_human cannot auto-route or pin-spend")
	}

	depth, err := canonicalDepth(seen["depth"])
	if err != nil {
		return ParsedRouteRequirement{}, err
	}
	perm, err := canonicalPermission(seen["permission"])
	if err != nil {
		return ParsedRouteRequirement{}, err
	}
	return ParsedRouteRequirement{Class: cl, Depth: depth, Permission: perm}, nil
}

func canonicalDepth(v string) (string, error) {
	d := strings.ToLower(strings.TrimSpace(v))
	switch d {
	case "low", "medium", "high":
		return d, nil
	default:
		return "", fmt.Errorf("route_requirement depth=%q invalid (allowed: low|medium|high)", v)
	}
}

func canonicalPermission(v string) (string, error) {
	// Exact canonical tokens only — no undocumented aliases.
	p := strings.TrimSpace(v)
	switch p {
	case "read-only", "bounded_write":
		return p, nil
	default:
		return "", fmt.Errorf("route_requirement permission=%q invalid (allowed exact: read-only|bounded_write)", v)
	}
}

// ChildAssignment is the expected pre-claim child execution contract.
type ChildAssignment struct {
	ExecutionPlanDigest string
	WorkItemID          string
	TaskClass           string // canonical class token
	Depth               string // low|medium|high
	Permission          string // read-only|bounded_write
	OutputContract      string
}

// ChildContractDigest builds a stable digest over the expected assignment.
// Returns error when any required field is missing/invalid — never hashes empties.
// Field order is fixed; raw route string token order does not affect the digest.
func ChildContractDigest(a ChildAssignment) (string, error) {
	exec := strings.TrimSpace(a.ExecutionPlanDigest)
	if exec == "" {
		return "", fmt.Errorf("routecontract: execution_plan_digest required")
	}
	wid := strings.TrimSpace(a.WorkItemID)
	if wid == "" {
		return "", fmt.Errorf("routecontract: work_item_id required")
	}
	cl := strings.ToLower(strings.TrimSpace(a.TaskClass))
	if cl == "" || !capclass.Class(cl).Valid() || cl == string(capclass.ClassNeedsHuman) {
		return "", fmt.Errorf("routecontract: task_class %q invalid", a.TaskClass)
	}
	depth, err := canonicalDepth(a.Depth)
	if err != nil {
		return "", fmt.Errorf("routecontract: %w", err)
	}
	perm, err := canonicalPermission(a.Permission)
	if err != nil {
		return "", fmt.Errorf("routecontract: %w", err)
	}
	outc := strings.TrimSpace(a.OutputContract)
	if outc == "" {
		return "", fmt.Errorf("routecontract: output_contract required")
	}
	payload := strings.Join([]string{
		"v1",
		exec,
		wid,
		cl,
		depth,
		perm,
		outc,
	}, "\n")
	sum := sha256.Sum256([]byte(payload))
	// Full sha256 hex — exact durable identity, never truncated.
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ValidateRouteMatchesParsed ensures resolved ChildRoute dimensions equal the
// parsed definition contract (exact class/depth/permission). No invent.
func ValidateRouteMatchesParsed(pr ParsedRouteRequirement, taskClass, depth, permission string) error {
	gotClass := strings.ToLower(strings.TrimSpace(taskClass))
	if gotClass != string(pr.Class) {
		return fmt.Errorf("routecontract: task_class mismatch route=%q definition=%q", taskClass, pr.Class)
	}
	gotDepth := strings.ToLower(strings.TrimSpace(depth))
	if gotDepth != pr.Depth {
		return fmt.Errorf("routecontract: depth mismatch route=%q definition=%q", depth, pr.Depth)
	}
	gotPerm := strings.TrimSpace(permission)
	if gotPerm != pr.Permission {
		return fmt.Errorf("routecontract: permission mismatch route=%q definition=%q", permission, pr.Permission)
	}
	return nil
}
