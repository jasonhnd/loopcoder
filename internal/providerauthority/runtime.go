package providerauthority

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/process"
	"github.com/jasonhnd/loopcoder/internal/runtimepath"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

const (
	StateActive           = "active"
	StateStale            = "stale"
	StateAmbiguous        = "ambiguous"
	StateTerminal         = "terminal"
	StateIdentityMismatch = "identity-mismatch"
	StateMissingRow       = "missing-row"
	StateCorruptRow       = "corrupt-row"
	StateUnregistered     = "unregistered"
)

type Runtime struct {
	ProjectID string
	Store     storage.Store
	Close     func()
}

type View struct {
	Authority       storage.ProviderExecutionAuthority
	State           string
	Reason          string
	Verified        bool
	OwnershipActive bool
}

func OpenRuntime(ctx context.Context, repoPath string, now func() time.Time) (Runtime, error) {
	roots, err := runtimepath.Resolve(ctx, repoPath)
	if err != nil {
		return Runtime{}, err
	}
	if !roots.Registered || strings.TrimSpace(roots.ProjectID) == "" || strings.TrimSpace(roots.DatabasePath) == "" {
		return Runtime{ProjectID: roots.ProjectID}, nil
	}
	store, err := storage.Open(ctx, storage.Options{Path: roots.DatabasePath, Now: now})
	if err != nil {
		return Runtime{}, err
	}
	return Runtime{ProjectID: roots.ProjectID, Store: store, Close: func() { _ = store.Close() }}, nil
}

func (r Runtime) Registered() bool {
	return r.Store != nil && strings.TrimSpace(r.ProjectID) != ""
}

func (r Runtime) List(ctx context.Context, runID string) ([]View, error) {
	if !r.Registered() {
		return nil, nil
	}
	authorities, err := storage.ListProviderExecutionAuthorities(ctx, r.Store, r.ProjectID, runID)
	if err != nil {
		return nil, err
	}
	views := make([]View, 0, len(authorities))
	for _, authority := range authorities {
		views = append(views, Inspect(authority))
	}
	return views, nil
}

func (r Runtime) Load(ctx context.Context, runID, attemptID string) (View, error) {
	if !r.Registered() {
		return View{State: StateUnregistered, Reason: "project is not registered in durable runtime storage"}, nil
	}
	authority, err := storage.LoadProviderExecutionAuthority(ctx, r.Store, r.ProjectID, runID, attemptID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Missing(runID, attemptID), nil
		}
		return View{State: StateCorruptRow, Reason: err.Error()}, nil
	}
	return Inspect(authority), nil
}

func (r Runtime) ValidateOwnership(ctx context.Context, view View, at time.Time) error {
	if !r.Registered() {
		return fmt.Errorf("provider authority runtime is not registered")
	}
	return storage.ValidateProviderExecutionAuthorityOwner(ctx, r.Store, storage.ProviderExecutionAuthorityFence{
		ProjectID:       view.Authority.ProjectID,
		RunID:           view.Authority.RunID,
		AttemptID:       view.Authority.AttemptID,
		OwnerID:         view.Authority.OwnerID,
		ClaimGeneration: view.Authority.ClaimGeneration,
	}, at)
}

func Inspect(authority storage.ProviderExecutionAuthority) View {
	view := View{Authority: authority}
	if strings.TrimSpace(authority.CompletedAt) != "" || strings.TrimSpace(authority.TerminalState) != "" {
		view.State = StateTerminal
		view.Reason = firstNonEmpty(authority.TerminalState, "authority row is terminal")
		return view
	}
	if authority.ProviderPID <= 0 || authority.ProviderPGID <= 0 || strings.TrimSpace(authority.ProcessBirthIdentity) == "" || strings.TrimSpace(authority.ExecutableIdentity) == "" || authority.IdentityAmbiguous {
		view.State = StateAmbiguous
		view.Reason = firstNonEmpty(authority.AmbiguityReason, "incomplete-process-identity")
		return view
	}
	err := process.VerifySnapshot(process.Identity{
		PID:                  authority.ProviderPID,
		PGID:                 authority.ProviderPGID,
		ProcessBirthIdentity: authority.ProcessBirthIdentity,
		ExecutableIdentity:   authority.ExecutableIdentity,
	})
	if err == nil {
		view.State = StateActive
		view.Reason = "verified"
		view.Verified = true
		return view
	}
	view.Reason = err.Error()
	if strings.Contains(view.Reason, "not alive") {
		view.State = StateStale
		return view
	}
	if strings.Contains(view.Reason, "ambiguous") {
		view.State = StateAmbiguous
		return view
	}
	view.State = StateIdentityMismatch
	return view
}

func Missing(runID, attemptID string) View {
	return View{
		Authority: storage.ProviderExecutionAuthority{
			RunID:     strings.TrimSpace(runID),
			AttemptID: strings.TrimSpace(attemptID),
		},
		State:  StateMissingRow,
		Reason: "provider execution authority row is missing",
	}
}

func WorktreeDisplay(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	return filepath.ToSlash(path)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
