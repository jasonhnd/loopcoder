package providerinventory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/models"
	"github.com/jasonhnd/loopcoder/internal/runtimecap"
)

const (
	ModelCatalogSnapshotSchema = "loopcoder.model_catalog_snapshot.v1"
	ModelCapabilitySchema      = "loopcoder.model_capability.v1"

	catalogStaleHorizon = 30 * 24 * time.Hour
)

type CatalogSourceKind string

const (
	CatalogSourceAdapterDeclared         CatalogSourceKind = "adapter-declared"
	CatalogSourceProviderMachineReadable CatalogSourceKind = "provider-machine-readable"
	CatalogSourceConfiguredOverlay       CatalogSourceKind = "configured-overlay"
	CatalogSourceFixture                 CatalogSourceKind = "fixture"
	CatalogSourceMigration               CatalogSourceKind = "migration"
)

type LifecycleState string

const (
	LifecycleAvailable  LifecycleState = "available"
	LifecycleRenamed    LifecycleState = "renamed"
	LifecycleDeprecated LifecycleState = "deprecated"
	LifecycleRemoved    LifecycleState = "removed"
)

type AvailabilityState string

const (
	AvailabilityAvailable              AvailabilityState = "available"
	AvailabilityAccountRestricted      AvailabilityState = "account-restricted"
	AvailabilityTemporarilyUnavailable AvailabilityState = "temporarily-unavailable"
	AvailabilityUnknown                AvailabilityState = "unknown"
	AvailabilityRemoved                AvailabilityState = "removed"
)

type CapabilityState string

const (
	CapabilityTrue    CapabilityState = "true"
	CapabilityFalse   CapabilityState = "false"
	CapabilityUnknown CapabilityState = "unknown"
)

type CatalogRole string

const (
	CatalogRoleWorker          CatalogRole = "worker"
	CatalogRoleVerifier        CatalogRole = "verifier"
	CatalogRoleAuditReview     CatalogRole = "audit-review"
	CatalogRoleNestedSubagents CatalogRole = "nested-subagents"
)

type ModelCatalogSnapshot struct {
	SchemaVersion          string            `json:"schema_version"`
	RecordVersion          int               `json:"record_version"`
	ModelCatalogSnapshotID string            `json:"model_catalog_snapshot_id"`
	AdapterID              string            `json:"adapter_id"`
	ProviderInstallationID *string           `json:"provider_installation_id,omitempty"`
	AccountProfileID       *string           `json:"account_profile_id,omitempty"`
	AuthReadinessID        *string           `json:"auth_readiness_id,omitempty"`
	CatalogSourceKind      CatalogSourceKind `json:"catalog_source_kind"`
	CatalogSourceReference string            `json:"catalog_source_reference"`
	SourceSchemaVersion    string            `json:"source_schema_version,omitempty"`
	ProviderCLIVersion     string            `json:"provider_cli_version,omitempty"`
	SourcePrecedence       int               `json:"source_precedence"`
	EntryCount             int               `json:"entry_count"`
	ConflictCount          int               `json:"conflict_count"`
	StalePolicy            string            `json:"stale_policy"`
	InventoryFingerprint   string            `json:"inventory_fingerprint"`
	CreatedAt              string            `json:"created_at"`
	UpdatedAt              string            `json:"updated_at"`
	CreatedBy              ActorProvenance   `json:"created_by"`
	UpdatedBy              ActorProvenance   `json:"updated_by"`
	Host                   HostProvenance    `json:"host"`
	PolicyVersion          string            `json:"policy_version"`
	CapturedAt             string            `json:"captured_at"`
	StaleAfter             string            `json:"stale_after,omitempty"`
	FreshnessState         FreshnessState    `json:"freshness_state"`
	Confidence             Confidence        `json:"confidence"`
	SideEffectClass        string            `json:"side_effect_class"`
	Classification         string            `json:"classification"`
	Source                 SourceDescriptor  `json:"source"`
	Evidence               EvidenceSummary   `json:"evidence"`
	GapReasons             []string          `json:"gap_reasons"`
	TerminalErrorCode      string            `json:"terminal_error_code,omitempty"`
}

type ModelCapability struct {
	SchemaVersion          string               `json:"schema_version"`
	RecordVersion          int                  `json:"record_version"`
	ModelCapabilityID      string               `json:"model_capability_id"`
	ModelCatalogSnapshotID string               `json:"model_catalog_snapshot_id"`
	AdapterID              string               `json:"adapter_id"`
	CanonicalModelID       string               `json:"canonical_model_id"`
	DisplayName            string               `json:"display_name,omitempty"`
	Aliases                []ModelAlias         `json:"aliases"`
	LifecycleState         LifecycleState       `json:"lifecycle_state"`
	ReplacementModelID     string               `json:"replacement_model_id,omitempty"`
	AvailabilityState      AvailabilityState    `json:"availability_state"`
	RolesSupported         []CatalogRole        `json:"roles_supported"`
	ReadOnly               CapabilityState      `json:"read_only"`
	JSONOutput             CapabilityState      `json:"json_output"`
	NestedSubagents        CapabilityState      `json:"nested_subagents"`
	MCPConfig              CapabilityState      `json:"mcp_config"`
	Cancellation           CapabilityState      `json:"cancellation"`
	TokenUsageReporting    CapabilityState      `json:"token_usage_reporting"`
	ContextWindowTokens    *CapabilityNumeric   `json:"context_window_tokens,omitempty"`
	ToolSupport            []CapabilityFact     `json:"tool_support,omitempty"`
	ImageInput             CapabilityState      `json:"image_input"`
	ImageOutput            CapabilityState      `json:"image_output"`
	Constraints            []string             `json:"constraints"`
	EntrySources           []CatalogEntrySource `json:"entry_sources"`
	Conflicts              []CatalogConflict    `json:"conflicts"`
	CreatedAt              string               `json:"created_at"`
	UpdatedAt              string               `json:"updated_at"`
	CreatedBy              ActorProvenance      `json:"created_by"`
	UpdatedBy              ActorProvenance      `json:"updated_by"`
	Host                   HostProvenance       `json:"host"`
	PolicyVersion          string               `json:"policy_version"`
	CapturedAt             string               `json:"captured_at"`
	StaleAfter             string               `json:"stale_after,omitempty"`
	FreshnessState         FreshnessState       `json:"freshness_state"`
	Confidence             Confidence           `json:"confidence"`
	SideEffectClass        string               `json:"side_effect_class"`
	Classification         string               `json:"classification"`
	Source                 SourceDescriptor     `json:"source"`
	Evidence               EvidenceSummary      `json:"evidence"`
	GapReasons             []string             `json:"gap_reasons"`
	TerminalErrorCode      string               `json:"terminal_error_code,omitempty"`
}

type ModelAlias struct {
	Alias      string             `json:"alias"`
	Source     CatalogEntrySource `json:"source"`
	Confidence Confidence         `json:"confidence"`
}

type CapabilityNumeric struct {
	Value      int                `json:"value"`
	Confidence Confidence         `json:"confidence"`
	Source     CatalogEntrySource `json:"source"`
}

type CapabilityFact struct {
	Name       string             `json:"name"`
	Value      string             `json:"value"`
	Confidence Confidence         `json:"confidence"`
	Source     CatalogEntrySource `json:"source"`
}

type CatalogEntrySource struct {
	SourceKind      CatalogSourceKind `json:"source_kind"`
	SourceReference string            `json:"source_reference"`
	Precedence      int               `json:"precedence"`
	Confidence      Confidence        `json:"confidence"`
	FreshnessState  FreshnessState    `json:"freshness_state"`
}

type CatalogConflict struct {
	Field                 string                  `json:"field"`
	Rule                  string                  `json:"rule"`
	ChosenSourceReference string                  `json:"chosen_source_reference"`
	Sources               []CatalogConflictSource `json:"sources"`
}

type CatalogConflictSource struct {
	SourceReference string `json:"source_reference"`
	Value           string `json:"value"`
	Precedence      int    `json:"precedence"`
}

type CatalogSourceInput struct {
	Kind                CatalogSourceKind
	Reference           string
	SourceSchemaVersion string
	ProviderCLIVersion  string
	Precedence          int
	Confidence          Confidence
	FreshnessState      FreshnessState
	Entries             []CatalogInputEntry
	Gaps                []string
}

type CatalogInputEntry struct {
	CanonicalModelID    string
	DisplayName         string
	Aliases             []string
	LifecycleState      LifecycleState
	ReplacementModelID  string
	AvailabilityState   AvailabilityState
	RolesSupported      []CatalogRole
	ReadOnly            CapabilityState
	JSONOutput          CapabilityState
	NestedSubagents     CapabilityState
	MCPConfig           CapabilityState
	Cancellation        CapabilityState
	TokenUsageReporting CapabilityState
	ImageInput          CapabilityState
	ImageOutput         CapabilityState
	Constraints         []string
}

type HardRequirement struct {
	ReadOnly            bool
	JSONOutput          bool
	NestedSubagents     bool
	MCPConfig           bool
	Cancellation        bool
	TokenUsageReporting bool
	ImageInput          bool
	ImageOutput         bool
}

func (k *CatalogSourceKind) UnmarshalJSON(data []byte) error {
	value, err := unmarshalEnumString(data, "catalog_source_kind")
	if err != nil {
		return err
	}
	switch CatalogSourceKind(value) {
	case CatalogSourceAdapterDeclared, CatalogSourceProviderMachineReadable, CatalogSourceConfiguredOverlay, CatalogSourceFixture, CatalogSourceMigration:
		*k = CatalogSourceKind(value)
		return nil
	default:
		return fmt.Errorf("%w: unknown catalog_source_kind %q", ErrInvalidRecord, value)
	}
}

func (s *LifecycleState) UnmarshalJSON(data []byte) error {
	value, err := unmarshalEnumString(data, "lifecycle_state")
	if err != nil {
		return err
	}
	switch LifecycleState(value) {
	case LifecycleAvailable, LifecycleRenamed, LifecycleDeprecated, LifecycleRemoved:
		*s = LifecycleState(value)
		return nil
	default:
		return fmt.Errorf("%w: unknown lifecycle_state %q", ErrInvalidRecord, value)
	}
}

func (s *AvailabilityState) UnmarshalJSON(data []byte) error {
	value, err := unmarshalEnumString(data, "availability_state")
	if err != nil {
		return err
	}
	switch AvailabilityState(value) {
	case AvailabilityAvailable, AvailabilityAccountRestricted, AvailabilityTemporarilyUnavailable, AvailabilityUnknown, AvailabilityRemoved:
		*s = AvailabilityState(value)
		return nil
	default:
		return fmt.Errorf("%w: unknown availability_state %q", ErrInvalidRecord, value)
	}
}

func (s *CapabilityState) UnmarshalJSON(data []byte) error {
	value, err := unmarshalEnumString(data, "capability_state")
	if err != nil {
		return err
	}
	switch CapabilityState(value) {
	case CapabilityTrue, CapabilityFalse, CapabilityUnknown:
		*s = CapabilityState(value)
		return nil
	default:
		return fmt.Errorf("%w: unknown capability_state %q", ErrInvalidRecord, value)
	}
}

func (c ModelCapability) SatisfiesHardRequirements(req HardRequirement) bool {
	if c.FreshnessState == FreshnessStale || c.FreshnessState == FreshnessExpired || c.Confidence == ConfidenceStale || c.Confidence == ConfidenceUnknown || c.Confidence == ConfidenceUnavailable {
		return false
	}
	if c.LifecycleState == LifecycleRemoved || c.AvailabilityState == AvailabilityRemoved || c.AvailabilityState == AvailabilityTemporarilyUnavailable || c.AvailabilityState == AvailabilityUnknown {
		return false
	}
	checks := []struct {
		required bool
		value    CapabilityState
	}{
		{req.ReadOnly, c.ReadOnly},
		{req.JSONOutput, c.JSONOutput},
		{req.NestedSubagents, c.NestedSubagents},
		{req.MCPConfig, c.MCPConfig},
		{req.Cancellation, c.Cancellation},
		{req.TokenUsageReporting, c.TokenUsageReporting},
		{req.ImageInput, c.ImageInput},
		{req.ImageOutput, c.ImageOutput},
	}
	for _, check := range checks {
		if check.required && check.value != CapabilityTrue {
			return false
		}
	}
	return true
}

func staticCatalogForAdapter(adapter AdapterDeclaration, now time.Time) (ModelCatalogSnapshot, []ModelCapability, error) {
	source := CatalogSourceInput{
		Kind:           CatalogSourceAdapterDeclared,
		Reference:      "loopcoder-static-registry:" + adapter.AdapterID,
		Precedence:     100,
		Confidence:     ConfidenceExact,
		FreshnessState: FreshnessFresh,
	}
	if len(adapter.StaticCatalogEntries) > 0 {
		source.Entries = append(source.Entries, adapter.StaticCatalogEntries...)
	} else if adapter.AdapterID != "grok" {
		if provider, ok := models.LookupProvider(adapter.AdapterID); ok {
			for _, model := range provider.Models {
				source.Entries = append(source.Entries, catalogEntryFromModel(adapter.AdapterID, model))
			}
		} else {
			source.Gaps = append(source.Gaps, "adapter-static-catalog-empty")
		}
	} else {
		source.Gaps = append(source.Gaps, "adapter-static-catalog-empty")
	}
	return buildCatalogSnapshot(adapter, nil, []CatalogSourceInput{source}, now)
}

func buildCatalogSnapshot(adapter AdapterDeclaration, providerInstallationID *string, sources []CatalogSourceInput, now time.Time) (ModelCatalogSnapshot, []ModelCapability, error) {
	now = now.UTC()
	nowText := formatTime(now)
	staleAfter := formatTime(now.Add(catalogStaleHorizon))
	if len(sources) == 0 {
		sources = []CatalogSourceInput{{
			Kind:           CatalogSourceAdapterDeclared,
			Reference:      "loopcoder-static-registry:" + adapter.AdapterID,
			Precedence:     100,
			Confidence:     ConfidenceUnavailable,
			FreshnessState: FreshnessFresh,
			Gaps:           []string{"adapter-static-catalog-empty"},
		}}
	}
	for index := range sources {
		sources[index] = normalizeCatalogSource(sources[index], adapter.AdapterID)
	}
	sort.SliceStable(sources, func(i, j int) bool {
		if sources[i].Precedence != sources[j].Precedence {
			return sources[i].Precedence > sources[j].Precedence
		}
		return sources[i].Reference < sources[j].Reference
	})
	snapshotID := catalogSnapshotID(adapter.AdapterID, sources)
	merged := mergeCatalogSources(snapshotID, adapter, sources, nowText, staleAfter)
	conflictCount := 0
	for _, entry := range merged {
		conflictCount += len(entry.Conflicts)
	}
	gaps := collectCatalogGaps(sources)
	sourceKind := sources[0].Kind
	sourceRef := sources[0].Reference
	sourceSchema := sources[0].SourceSchemaVersion
	cliVersion := sources[0].ProviderCLIVersion
	snapshot := ModelCatalogSnapshot{
		SchemaVersion:          ModelCatalogSnapshotSchema,
		RecordVersion:          1,
		ModelCatalogSnapshotID: snapshotID,
		AdapterID:              adapter.AdapterID,
		ProviderInstallationID: providerInstallationID,
		CatalogSourceKind:      sourceKind,
		CatalogSourceReference: sourceRef,
		SourceSchemaVersion:    sourceSchema,
		ProviderCLIVersion:     cliVersion,
		SourcePrecedence:       sources[0].Precedence,
		EntryCount:             len(merged),
		ConflictCount:          conflictCount,
		StalePolicy:            "static-adapter-catalog-30d",
		CreatedAt:              nowText,
		UpdatedAt:              nowText,
		CreatedBy:              defaultActorProvenance(),
		UpdatedBy:              defaultActorProvenance(),
		Host:                   hostProvenance(),
		PolicyVersion:          PolicyVersion,
		CapturedAt:             nowText,
		StaleAfter:             staleAfter,
		FreshnessState:         FreshnessFresh,
		Confidence:             confidenceForCatalog(sources),
		SideEffectClass:        "local-read",
		Classification:         "public-provider-metadata",
		Source:                 SourceDescriptor{Kind: string(sourceKind), AdapterID: adapter.AdapterID},
		Evidence:               EvidenceSummary{Kind: "adapter-declared-catalog", CommandBounded: true, NoShell: true, RepositoryMutation: false, SecretMaterialRetained: false},
		GapReasons:             gaps,
	}
	fingerprint, err := catalogFingerprint(snapshot, merged)
	if err != nil {
		return ModelCatalogSnapshot{}, nil, err
	}
	snapshot.InventoryFingerprint = fingerprint
	return snapshot, merged, nil
}

func catalogEntryFromModel(adapterID string, model models.Model) CatalogInputEntry {
	provider, providerOK := runtimecap.LookupProvider(adapterID)
	entry := CatalogInputEntry{
		CanonicalModelID: model.Name,
		DisplayName:      model.Name,
		LifecycleState:   LifecycleAvailable,
		// Adapter-declared static registry models are product-available for routing.
		// Account restriction and auth readiness remain separate gates.
		AvailabilityState:   AvailabilityAvailable,
		ReadOnly:            CapabilityUnknown,
		JSONOutput:          CapabilityUnknown,
		NestedSubagents:     CapabilityUnknown,
		MCPConfig:           CapabilityUnknown,
		Cancellation:        CapabilityUnknown,
		TokenUsageReporting: CapabilityUnknown,
		ImageInput:          CapabilityUnknown,
		ImageOutput:         CapabilityUnknown,
	}
	if providerOK {
		entry.ReadOnly = boolCapability(provider.ReadOnly)
		entry.JSONOutput = boolCapability(provider.JSONOutput)
		entry.NestedSubagents = boolCapability(provider.NestedSubagents)
		entry.MCPConfig = boolCapability(provider.MCPConfig)
		entry.Cancellation = boolCapability(provider.Cancellation)
		entry.TokenUsageReporting = boolCapability(provider.TokenUsageReporting)
		entry.RolesSupported = rolesForProvider(provider)
	}
	return entry
}

func normalizeCatalogSource(source CatalogSourceInput, adapterID string) CatalogSourceInput {
	if source.Kind == "" {
		source.Kind = CatalogSourceAdapterDeclared
	}
	if strings.TrimSpace(source.Reference) == "" {
		source.Reference = string(source.Kind) + ":" + adapterID
	}
	if source.Confidence == "" {
		source.Confidence = ConfidenceUnknown
	}
	if source.FreshnessState == "" {
		source.FreshnessState = FreshnessFresh
	}
	if source.Gaps == nil {
		source.Gaps = []string{}
	}
	return source
}

func mergeCatalogSources(snapshotID string, adapter AdapterDeclaration, sources []CatalogSourceInput, nowText, staleAfter string) []ModelCapability {
	byModel := map[string][]catalogSourcedEntry{}
	for _, source := range sources {
		entrySource := CatalogEntrySource{
			SourceKind:      source.Kind,
			SourceReference: source.Reference,
			Precedence:      source.Precedence,
			Confidence:      source.Confidence,
			FreshnessState:  source.FreshnessState,
		}
		for _, input := range source.Entries {
			input = normalizeCatalogInput(input)
			if input.CanonicalModelID == "" {
				continue
			}
			byModel[input.CanonicalModelID] = append(byModel[input.CanonicalModelID], catalogSourcedEntry{input: input, source: entrySource})
		}
	}
	models := make([]string, 0, len(byModel))
	for modelID := range byModel {
		models = append(models, modelID)
	}
	sort.Strings(models)
	out := make([]ModelCapability, 0, len(models))
	for _, modelID := range models {
		entries := byModel[modelID]
		sort.SliceStable(entries, func(i, j int) bool {
			if entries[i].source.Precedence != entries[j].source.Precedence {
				return entries[i].source.Precedence > entries[j].source.Precedence
			}
			return entries[i].source.SourceReference < entries[j].source.SourceReference
		})
		chosen := entries[0].input
		sources := make([]CatalogEntrySource, 0, len(entries))
		for _, entry := range entries {
			sources = append(sources, entry.source)
		}
		conflicts := catalogConflicts(entries)
		aliases := modelAliases(chosen.Aliases, sources[0])
		capability := ModelCapability{
			SchemaVersion:          ModelCapabilitySchema,
			RecordVersion:          1,
			ModelCapabilityID:      modelCapabilityID(snapshotID, chosen.CanonicalModelID, chosen.LifecycleState),
			ModelCatalogSnapshotID: snapshotID,
			AdapterID:              adapter.AdapterID,
			CanonicalModelID:       chosen.CanonicalModelID,
			DisplayName:            chosen.DisplayName,
			Aliases:                aliases,
			LifecycleState:         chosen.LifecycleState,
			ReplacementModelID:     chosen.ReplacementModelID,
			AvailabilityState:      chosen.AvailabilityState,
			RolesSupported:         append([]CatalogRole(nil), chosen.RolesSupported...),
			ReadOnly:               chosen.ReadOnly,
			JSONOutput:             chosen.JSONOutput,
			NestedSubagents:        chosen.NestedSubagents,
			MCPConfig:              chosen.MCPConfig,
			Cancellation:           chosen.Cancellation,
			TokenUsageReporting:    chosen.TokenUsageReporting,
			ImageInput:             chosen.ImageInput,
			ImageOutput:            chosen.ImageOutput,
			Constraints:            append([]string(nil), chosen.Constraints...),
			EntrySources:           sources,
			Conflicts:              conflicts,
			CreatedAt:              nowText,
			UpdatedAt:              nowText,
			CreatedBy:              defaultActorProvenance(),
			UpdatedBy:              defaultActorProvenance(),
			Host:                   hostProvenance(),
			PolicyVersion:          PolicyVersion,
			CapturedAt:             nowText,
			StaleAfter:             staleAfter,
			FreshnessState:         freshnessForSources(sources),
			Confidence:             confidenceForSources(sources),
			SideEffectClass:        "local-read",
			Classification:         "public-provider-metadata",
			Source:                 SourceDescriptor{Kind: string(sources[0].SourceKind), AdapterID: adapter.AdapterID},
			Evidence:               EvidenceSummary{Kind: "catalog-entry", CommandBounded: true, NoShell: true, RepositoryMutation: false, SecretMaterialRetained: false},
			GapReasons:             []string{},
		}
		if capability.FreshnessState == FreshnessStale || capability.FreshnessState == FreshnessExpired {
			capability.Confidence = ConfidenceStale
		}
		out = append(out, capability)
	}
	return out
}

type catalogSourcedEntry struct {
	input  CatalogInputEntry
	source CatalogEntrySource
}

func normalizeCatalogInput(input CatalogInputEntry) CatalogInputEntry {
	input.CanonicalModelID = strings.TrimSpace(input.CanonicalModelID)
	if input.DisplayName == "" {
		input.DisplayName = input.CanonicalModelID
	}
	if input.LifecycleState == "" {
		input.LifecycleState = LifecycleAvailable
	}
	if input.AvailabilityState == "" {
		input.AvailabilityState = AvailabilityUnknown
	}
	for _, field := range []*CapabilityState{
		&input.ReadOnly, &input.JSONOutput, &input.NestedSubagents, &input.MCPConfig,
		&input.Cancellation, &input.TokenUsageReporting, &input.ImageInput, &input.ImageOutput,
	} {
		if *field == "" {
			*field = CapabilityUnknown
		}
	}
	input.Aliases = dedupeStrings(input.Aliases)
	input.Constraints = dedupeStrings(input.Constraints)
	sort.Slice(input.RolesSupported, func(i, j int) bool { return input.RolesSupported[i] < input.RolesSupported[j] })
	return input
}

func catalogConflicts(entries []catalogSourcedEntry) []CatalogConflict {
	fields := []struct {
		name string
		val  func(CatalogInputEntry) string
	}{
		{"lifecycle_state", func(e CatalogInputEntry) string { return string(e.LifecycleState) }},
		{"availability_state", func(e CatalogInputEntry) string { return string(e.AvailabilityState) }},
		{"replacement_model_id", func(e CatalogInputEntry) string { return e.ReplacementModelID }},
		{"read_only", func(e CatalogInputEntry) string { return string(e.ReadOnly) }},
		{"json_output", func(e CatalogInputEntry) string { return string(e.JSONOutput) }},
		{"nested_subagents", func(e CatalogInputEntry) string { return string(e.NestedSubagents) }},
		{"mcp_config", func(e CatalogInputEntry) string { return string(e.MCPConfig) }},
		{"cancellation", func(e CatalogInputEntry) string { return string(e.Cancellation) }},
		{"token_usage_reporting", func(e CatalogInputEntry) string { return string(e.TokenUsageReporting) }},
	}
	var conflicts []CatalogConflict
	for _, field := range fields {
		values := map[string]bool{}
		for _, entry := range entries {
			values[field.val(entry.input)] = true
		}
		if len(values) < 2 {
			continue
		}
		sources := make([]CatalogConflictSource, 0, len(entries))
		for _, entry := range entries {
			sources = append(sources, CatalogConflictSource{
				SourceReference: entry.source.SourceReference,
				Value:           field.val(entry.input),
				Precedence:      entry.source.Precedence,
			})
		}
		conflicts = append(conflicts, CatalogConflict{
			Field:                 field.name,
			Rule:                  "highest-source-precedence-wins; all incompatible source values are preserved",
			ChosenSourceReference: entries[0].source.SourceReference,
			Sources:               sources,
		})
	}
	return conflicts
}

func modelAliases(aliases []string, source CatalogEntrySource) []ModelAlias {
	aliases = dedupeStrings(aliases)
	out := make([]ModelAlias, 0, len(aliases))
	for _, alias := range aliases {
		out = append(out, ModelAlias{Alias: alias, Source: source, Confidence: source.Confidence})
	}
	return out
}

func freshnessForSources(sources []CatalogEntrySource) FreshnessState {
	for _, source := range sources {
		if source.FreshnessState == FreshnessFresh {
			return FreshnessFresh
		}
	}
	for _, source := range sources {
		if source.FreshnessState == FreshnessStale {
			return FreshnessStale
		}
	}
	return FreshnessNotApplicable
}

func confidenceForSources(sources []CatalogEntrySource) Confidence {
	for _, source := range sources {
		if source.FreshnessState == FreshnessStale || source.FreshnessState == FreshnessExpired {
			return ConfidenceStale
		}
	}
	for _, source := range sources {
		if source.Confidence == ConfidenceExact {
			return ConfidenceExact
		}
	}
	for _, source := range sources {
		if source.Confidence == ConfidenceEstimated {
			return ConfidenceEstimated
		}
	}
	for _, source := range sources {
		if source.Confidence == ConfidenceUnavailable {
			return ConfidenceUnavailable
		}
	}
	return ConfidenceUnknown
}

func confidenceForCatalog(sources []CatalogSourceInput) Confidence {
	entrySources := make([]CatalogEntrySource, 0, len(sources))
	for _, source := range sources {
		entrySources = append(entrySources, CatalogEntrySource{Confidence: source.Confidence, FreshnessState: source.FreshnessState})
	}
	return confidenceForSources(entrySources)
}

func collectCatalogGaps(sources []CatalogSourceInput) []string {
	var gaps []string
	for _, source := range sources {
		gaps = append(gaps, source.Gaps...)
	}
	return dedupeStrings(gaps)
}

func catalogSnapshotID(adapterID string, sources []CatalogSourceInput) string {
	parts := []string{adapterID}
	for _, source := range sources {
		parts = append(parts, string(source.Kind), source.Reference, fmt.Sprintf("%d", source.Precedence))
		for _, entry := range source.Entries {
			parts = append(parts, entry.CanonicalModelID, string(entry.LifecycleState), string(entry.AvailabilityState), entry.ReplacementModelID)
		}
	}
	return "mcatsnap_" + hashBase32(parts...)[:32]
}

func modelCapabilityID(snapshotID, canonicalModelID string, lifecycle LifecycleState) string {
	return "mcap_" + hashBase32(snapshotID, canonicalModelID, string(lifecycle))[:32]
}

func catalogFingerprint(snapshot ModelCatalogSnapshot, capabilities []ModelCapability) (string, error) {
	snapshot.InventoryFingerprint = ""
	payload := struct {
		Snapshot     ModelCatalogSnapshot `json:"snapshot"`
		Capabilities []ModelCapability    `json:"capabilities"`
	}{Snapshot: snapshot, Capabilities: capabilities}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func boolCapability(value bool) CapabilityState {
	if value {
		return CapabilityTrue
	}
	return CapabilityFalse
}

func rolesForProvider(provider runtimecap.ProviderRuntime) []CatalogRole {
	var roles []CatalogRole
	if provider.Cancellation {
		roles = append(roles, CatalogRoleWorker)
	}
	if provider.ReadOnly && provider.JSONOutput && provider.Cancellation {
		roles = append(roles, CatalogRoleVerifier, CatalogRoleAuditReview)
	}
	if provider.NestedSubagents && provider.Cancellation {
		roles = append(roles, CatalogRoleNestedSubagents)
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i] < roles[j] })
	return roles
}

func dedupeStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func markCatalogFreshness(snapshot ModelCatalogSnapshot, capabilities []ModelCapability, now time.Time) (ModelCatalogSnapshot, []ModelCapability) {
	if !catalogExpired(snapshot.StaleAfter, now) {
		return snapshot, capabilities
	}
	snapshot.FreshnessState = FreshnessStale
	snapshot.Confidence = ConfidenceStale
	for i := range capabilities {
		if capabilities[i].ModelCatalogSnapshotID != snapshot.ModelCatalogSnapshotID {
			continue
		}
		capabilities[i].FreshnessState = FreshnessStale
		capabilities[i].Confidence = ConfidenceStale
	}
	return snapshot, capabilities
}

func catalogExpired(staleAfter string, now time.Time) bool {
	if strings.TrimSpace(staleAfter) == "" {
		return false
	}
	cutoff, err := time.Parse(time.RFC3339Nano, staleAfter)
	if err != nil {
		return true
	}
	return now.UTC().After(cutoff)
}
