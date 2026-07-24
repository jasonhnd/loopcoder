// Package execidentity builds canonical execution identity for direct-run and
// related single-node product paths. Digests are derived only from real contract
// inputs via workflowdef Normalize/Materialize and routecontract — never from
// ad hoc CLI hashes or silent class defaults.
package execidentity

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/routecontract"
	"github.com/jasonhnd/loopcoder/internal/workflowdef"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// DirectRunOutputContract is the product-documented output contract for a
// single-node direct run (branch + diff evidence). Callers pass this explicitly.
const DirectRunOutputContract = "branch+diff"

// DirectWorkItemID is the single work item id for a direct-run graph.
const DirectWorkItemID = "only"

// DirectContractInput is the real product inputs required to freeze identity.
// Every field is required nonempty/valid — no silent defaults (including class).
type DirectContractInput struct {
	// IssueTitle and IssueBody are the real issue payload (not "issue N").
	IssueTitle string
	IssueBody  string
	// BaseSHA is the exact git commit the run materializes from.
	BaseSHA string
	// TaskClass is the classified capability floor (luna|tera|soul). Required.
	TaskClass string
	// Depth is canonical low|medium|high from classification/selection.
	Depth string
	// Permission is exact read-only|bounded_write.
	Permission string
	// OutputContract is the explicit product output contract token.
	OutputContract string
	// Actor approves the materialize (default "owner" only when caller sets it).
	Actor string
	// ProjectID scopes materialize registry key (required nonempty).
	ProjectID string
	// GraphID optional stable graph id; empty → deterministic from base+title.
	GraphID string
	// Now for approval timestamp (CreatedAt excluded from graph digest).
	Now time.Time
}

// DirectContract is the frozen canonical identity for one direct run.
type DirectContract struct {
	// PlanDigest is workflowdef.Normalize digest (ExecutionPlanDigest).
	PlanDigest string
	// GraphDigest is workgraph.DigestGraph of the approved materialized graph.
	GraphDigest string
	TaskClass   string
	Depth       string
	Permission  string
	// OutputContract is the explicit contract token used in ChildContractDigest.
	OutputContract      string
	ChildContractDigest string
	WorkItemID          string
	// Definition is the normalized single-node definition (for reopen equality).
	Definition workflowdef.Definition
	// Graph is the approved materialized graph.
	Graph workgraph.Graph
}

// BuildDirectContract compiles a single-node execution contract from real inputs.
// Fail closed on any missing/invalid field — never invent digests or default class.
func BuildDirectContract(in DirectContractInput) (DirectContract, error) {
	title := strings.TrimSpace(in.IssueTitle)
	body := strings.TrimSpace(in.IssueBody)
	if title == "" && body == "" {
		return DirectContract{}, fmt.Errorf("execidentity: issue title/body required")
	}
	baseSHA := strings.TrimSpace(in.BaseSHA)
	if baseSHA == "" {
		return DirectContract{}, fmt.Errorf("execidentity: base_sha required")
	}
	// No silent Tera / empty class.
	cl := strings.ToLower(strings.TrimSpace(in.TaskClass))
	if cl == "" {
		return DirectContract{}, fmt.Errorf("execidentity: task_class required (no default)")
	}
	if !capclass.Class(cl).Valid() || cl == string(capclass.ClassNeedsHuman) {
		return DirectContract{}, fmt.Errorf("execidentity: task_class %q invalid", in.TaskClass)
	}
	depth := strings.ToLower(strings.TrimSpace(in.Depth))
	switch depth {
	case "low", "medium", "high":
	default:
		return DirectContract{}, fmt.Errorf("execidentity: depth %q invalid (want low|medium|high)", in.Depth)
	}
	perm := strings.TrimSpace(in.Permission)
	switch perm {
	case "read-only", "bounded_write":
	default:
		return DirectContract{}, fmt.Errorf("execidentity: permission %q invalid (want exact read-only|bounded_write)", in.Permission)
	}
	outc := strings.TrimSpace(in.OutputContract)
	if outc == "" {
		return DirectContract{}, fmt.Errorf("execidentity: output_contract required (no invent)")
	}
	actor := strings.TrimSpace(in.Actor)
	if actor == "" {
		return DirectContract{}, fmt.Errorf("execidentity: actor required")
	}
	projectID := strings.TrimSpace(in.ProjectID)
	if projectID == "" {
		return DirectContract{}, fmt.Errorf("execidentity: project_id required")
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	// Intent includes base SHA so plan digest binds the exact materialize commit.
	intent := "base_sha=" + baseSHA + "\n" + title
	if body != "" {
		intent += "\n\n" + body
	}
	routeReq := fmt.Sprintf("class=%s,depth=%s,permission=%s", cl, depth, perm)
	// Validate route requirement with shared parser before Normalize.
	if _, err := routecontract.ParseRouteRequirement(routeReq); err != nil {
		return DirectContract{}, fmt.Errorf("execidentity: route_requirement: %w", err)
	}

	graphID := strings.TrimSpace(in.GraphID)
	if graphID == "" {
		graphID = "g_direct_" + shortStable(baseSHA+"|"+title+"|"+cl+"|"+depth+"|"+perm+"|"+outc)
	}

	def := workflowdef.Definition{
		SchemaVersion: 1, GraphID: graphID,
		// Established workgraph source — never free-form "direct_run".
		Source: string(workgraph.SourceDirectMaterialize),
		Items: []workflowdef.DefItem{{
			ID: DirectWorkItemID, Intent: intent, Status: "required",
			IntegrationOrder: 1,
			RouteRequirement: routeReq,
			OutputContract:   outc,
		}},
	}
	plan, err := workflowdef.Normalize(def)
	if err != nil {
		return DirectContract{}, fmt.Errorf("execidentity: normalize: %w", err)
	}
	if strings.TrimSpace(plan.Digest) == "" {
		return DirectContract{}, fmt.Errorf("execidentity: empty plan digest")
	}
	ap, err := workflowdef.Approve(plan.Digest, actor, "direct run single-node", now)
	if err != nil {
		return DirectContract{}, fmt.Errorf("execidentity: approve: %w", err)
	}
	reg := workflowdef.NewRegistry()
	mat, err := reg.Materialize(projectID, def, ap, now)
	if err != nil {
		return DirectContract{}, fmt.Errorf("execidentity: materialize: %w", err)
	}
	g := mat.Graph
	graphDigest := workgraph.DigestGraph(g)
	if graphDigest == "" {
		return DirectContract{}, fmt.Errorf("execidentity: empty graph digest")
	}
	if stored := strings.TrimSpace(g.PlanDigest); stored != "" && stored != graphDigest {
		return DirectContract{}, fmt.Errorf("execidentity: graph PlanDigest %q != DigestGraph %q", stored, graphDigest)
	}
	if stored := strings.TrimSpace(g.PlanDigest); stored != "" {
		graphDigest = stored
	}

	ccd, err := routecontract.ChildContractDigest(routecontract.ChildAssignment{
		ExecutionPlanDigest: plan.Digest,
		WorkItemID:          DirectWorkItemID,
		TaskClass:           cl,
		Depth:               depth,
		Permission:          perm,
		OutputContract:      outc,
	})
	if err != nil {
		return DirectContract{}, fmt.Errorf("execidentity: child contract: %w", err)
	}
	if !fullSHA256Hex(ccd) {
		return DirectContract{}, fmt.Errorf("execidentity: child contract digest not full sha256: %q", ccd)
	}

	return DirectContract{
		PlanDigest:          plan.Digest,
		GraphDigest:         graphDigest,
		TaskClass:           cl,
		Depth:               depth,
		Permission:          perm,
		OutputContract:      outc,
		ChildContractDigest: ccd,
		WorkItemID:          DirectWorkItemID,
		Definition:          def,
		Graph:               g,
	}, nil
}

func fullSHA256Hex(d string) bool {
	const p = "sha256:"
	if !strings.HasPrefix(d, p) {
		return false
	}
	h := strings.TrimPrefix(d, p)
	if len(h) != 64 {
		return false
	}
	for _, c := range h {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func shortStable(s string) string {
	// GraphID convenience only — never a substitute for PlanDigest/GraphDigest/CCD.
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
