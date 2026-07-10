// Package defaults owns loopcoder's behavior-preserving built-in defaults and
// limits.
//
// This package is intentionally a low-level leaf: it imports only the standard
// library so higher-level packages can depend on it without import cycles.
package defaults

import "time"

const (
	// BaseBranch is the default delivery base branch.
	BaseBranch = "main"
	// PreProdBranch is the default unattended integration branch.
	PreProdBranch = "pre-prod"
	// DispatchWaveThrottleLimit is the default concurrent dispatch cap.
	DispatchWaveThrottleLimit = 4
	// NestedSchedulerMaxDepth is the default child-run depth cap.
	NestedSchedulerMaxDepth = 2
	// NestedSchedulerMaxChildren is the default child-run fan-out cap.
	NestedSchedulerMaxChildren = 16

	// WorkerHardCap is the worker provider watchdog hard cap.
	WorkerHardCap = 45 * time.Minute
	// WorkerStallTimeout is the worker provider no-progress timeout.
	WorkerStallTimeout = 5 * time.Minute

	WorkerHeartbeatIntervalSeconds = 15
	WorkerStaleAfterSeconds        = 120
	WorkerHungAfterSeconds         = 300
	WorkerMaxAttempts              = 3
	WorkerHardCapSeconds           = 2700
	WorkerStallTimeoutSeconds      = 300

	VerifierHardCapSeconds      = 900
	VerifierStallTimeoutSeconds = 300
	VerifierTimeout             = 15 * time.Minute
	VerifierStallTimeout        = 5 * time.Minute

	VerificationMaxFixPasses = 3
	VerificationBrowserMode  = "auto"

	GitCommandHardCap         = 60 * time.Second
	DoctorCommandHardCap      = 60 * time.Second
	ScaffoldGitHubCommandCap  = 60 * time.Second
	UpgradeCommandHardCap     = 60 * time.Second
	VerifyCommandHardCap      = 15 * time.Minute
	CompileGoListHardCap      = 120 * time.Second
	GitHubCommandHardCap      = 60 * time.Second
	SupervisedExecHardCap     = 30 * time.Minute
	ProcessLivenessCommandCap = 5 * time.Second
	UpgradeHTTPTimeout        = 60 * time.Second

	ReviewPacketChangedFilesBudgetBytes     = 8 * 1024
	ReviewPacketDiffBudgetBytes             = 80 * 1024
	ReviewPacketDiffFileBudgetBytes         = 24 * 1024
	ReviewPacketDocumentationBodyFileBytes  = 64 * 1024
	ReviewPacketDocumentationBodyTotalBytes = 96 * 1024
	ReviewPacketDocumentationBodyMaxFiles   = 3
	ReviewPacketGeneratedDiffFileBytes      = 2 * 1024
	ReviewPacketGeneratedSizeBytes          = 128 * 1024
	ReviewPacketIssueBudgetBytes            = 12 * 1024
	ReviewPacketRenderedArtifactBudgetBytes = 24 * 1024
	ReviewPacketRubricBudgetBytes           = 24 * 1024
	ReviewPacketSpecBudgetBytes             = 40 * 1024
	ReviewPacketTotalPromptBudgetBytes      = 160 * 1024
	RenderedArtifactFileBudgetBytes         = 8 * 1024
	RenderedArtifactMaxDirectoryFiles       = 32
	RenderedArtifactProducerTimeout         = 5 * time.Minute
	ProducerFailureLogBudgetBytes           = 4 * 1024
	ProviderFailureLogBudgetBytes           = 4 * 1024

	WorktreeLivenessMaxFiles     = 20000
	CustomLivenessOutputMaxBytes = 4096
	GitHubListLimit              = 1000

	ClaudeHookTimeoutSeconds       = 10
	HookInputMaxBytes        int64 = 1 << 20

	ConductorHookMaxStateFiles       = 128
	ConductorHookMaxStateAgeMillis   = int64(7) * 24 * 60 * 60 * 1000
	ConductorHookMaxTextField        = 4096
	ConductorHookMaxStateRecords     = 256
	ConductorHookMaxLedgerFiles      = 256
	ConductorHookMaxLedgerBytes      = int64(256 * 1024)
	ConductorHookRecentLedgerGraceMs = int64(10 * 60 * 1000)

	RunStatusMaxEventBytes       int64 = 64 << 20
	RunStatusMaxEventLineBytes   int   = 1 << 20
	RunStatusMaxRecordBytes      int64 = 4 << 20
	RunStatusMaxDirectoryEntries       = 4096
	RunStatusMaxFutureSkew             = 24 * time.Hour

	RelayGateMaxRecordSize = 256 * 1024

	StateBranchDefaultBranch     = "loopcoder/state"
	StateBranchDefaultRemote     = "origin"
	StateBranchDefaultTTLSeconds = 600
	StateBranchLogTailLines      = 50
)

var (
	workerRetryBackoffSeconds     = []int{10, 30, 120}
	reviewPacketGeneratedPatterns = []string{
		"tests/baseline/**",
		"*.lock",
		"dist/**",
		"*.min.*",
		"vendor/**",
	}
)

// Values is a snapshot of loopcoder's built-in defaults and limits.
type Values struct {
	BaseBranch                              string
	PreProdBranch                           string
	DispatchWaveThrottleLimit               int
	NestedSchedulerMaxDepth                 int
	NestedSchedulerMaxChildren              int
	WorkerHardCap                           time.Duration
	WorkerStallTimeout                      time.Duration
	WorkerHeartbeatIntervalSeconds          int
	WorkerStaleAfterSeconds                 int
	WorkerHungAfterSeconds                  int
	WorkerMaxAttempts                       int
	WorkerRetryBackoffSeconds               []int
	WorkerHardCapSeconds                    int
	WorkerStallTimeoutSeconds               int
	VerifierHardCapSeconds                  int
	VerifierStallTimeoutSeconds             int
	VerifierTimeout                         time.Duration
	VerifierStallTimeout                    time.Duration
	VerificationMaxFixPasses                int
	VerificationBrowserMode                 string
	GitCommandHardCap                       time.Duration
	DoctorCommandHardCap                    time.Duration
	ScaffoldGitHubCommandCap                time.Duration
	UpgradeCommandHardCap                   time.Duration
	VerifyCommandHardCap                    time.Duration
	CompileGoListHardCap                    time.Duration
	GitHubCommandHardCap                    time.Duration
	SupervisedExecHardCap                   time.Duration
	ProcessLivenessCommandCap               time.Duration
	UpgradeHTTPTimeout                      time.Duration
	ReviewPacketChangedFilesBudgetBytes     int
	ReviewPacketDiffBudgetBytes             int
	ReviewPacketDiffFileBudgetBytes         int
	ReviewPacketDocumentationBodyFileBytes  int
	ReviewPacketDocumentationBodyTotalBytes int
	ReviewPacketDocumentationBodyMaxFiles   int
	ReviewPacketGeneratedDiffFileBytes      int
	ReviewPacketGeneratedSizeBytes          int
	ReviewPacketGeneratedPatterns           []string
	ReviewPacketIssueBudgetBytes            int
	ReviewPacketRenderedArtifactBudgetBytes int
	ReviewPacketRubricBudgetBytes           int
	ReviewPacketSpecBudgetBytes             int
	ReviewPacketTotalPromptBudgetBytes      int
	RenderedArtifactFileBudgetBytes         int
	RenderedArtifactMaxDirectoryFiles       int
	RenderedArtifactProducerTimeout         time.Duration
	ProducerFailureLogBudgetBytes           int
	ProviderFailureLogBudgetBytes           int
	WorktreeLivenessMaxFiles                int
	CustomLivenessOutputMaxBytes            int
	GitHubListLimit                         int
	ClaudeHookTimeoutSeconds                int
	HookInputMaxBytes                       int64
	ConductorHookMaxStateFiles              int
	ConductorHookMaxStateAgeMillis          int64
	ConductorHookMaxTextField               int
	ConductorHookMaxStateRecords            int
	ConductorHookMaxLedgerFiles             int
	ConductorHookMaxLedgerBytes             int64
	ConductorHookRecentLedgerGraceMs        int64
	RunStatusMaxEventBytes                  int64
	RunStatusMaxEventLineBytes              int
	RunStatusMaxRecordBytes                 int64
	RunStatusMaxDirectoryEntries            int
	RunStatusMaxFutureSkew                  time.Duration
	RelayGateMaxRecordSize                  int
	StateBranchDefaultBranch                string
	StateBranchDefaultRemote                string
	StateBranchDefaultTTLSeconds            int
	StateBranchLogTailLines                 int
}

// Current returns a copy of the current built-in defaults.
func Current() Values {
	return Values{
		BaseBranch:                              BaseBranch,
		PreProdBranch:                           PreProdBranch,
		DispatchWaveThrottleLimit:               DispatchWaveThrottleLimit,
		NestedSchedulerMaxDepth:                 NestedSchedulerMaxDepth,
		NestedSchedulerMaxChildren:              NestedSchedulerMaxChildren,
		WorkerHardCap:                           WorkerHardCap,
		WorkerStallTimeout:                      WorkerStallTimeout,
		WorkerHeartbeatIntervalSeconds:          WorkerHeartbeatIntervalSeconds,
		WorkerStaleAfterSeconds:                 WorkerStaleAfterSeconds,
		WorkerHungAfterSeconds:                  WorkerHungAfterSeconds,
		WorkerMaxAttempts:                       WorkerMaxAttempts,
		WorkerRetryBackoffSeconds:               WorkerRetryBackoffSeconds(),
		WorkerHardCapSeconds:                    WorkerHardCapSeconds,
		WorkerStallTimeoutSeconds:               WorkerStallTimeoutSeconds,
		VerifierHardCapSeconds:                  VerifierHardCapSeconds,
		VerifierStallTimeoutSeconds:             VerifierStallTimeoutSeconds,
		VerifierTimeout:                         VerifierTimeout,
		VerifierStallTimeout:                    VerifierStallTimeout,
		VerificationMaxFixPasses:                VerificationMaxFixPasses,
		VerificationBrowserMode:                 VerificationBrowserMode,
		GitCommandHardCap:                       GitCommandHardCap,
		DoctorCommandHardCap:                    DoctorCommandHardCap,
		ScaffoldGitHubCommandCap:                ScaffoldGitHubCommandCap,
		UpgradeCommandHardCap:                   UpgradeCommandHardCap,
		VerifyCommandHardCap:                    VerifyCommandHardCap,
		CompileGoListHardCap:                    CompileGoListHardCap,
		GitHubCommandHardCap:                    GitHubCommandHardCap,
		SupervisedExecHardCap:                   SupervisedExecHardCap,
		ProcessLivenessCommandCap:               ProcessLivenessCommandCap,
		UpgradeHTTPTimeout:                      UpgradeHTTPTimeout,
		ReviewPacketChangedFilesBudgetBytes:     ReviewPacketChangedFilesBudgetBytes,
		ReviewPacketDiffBudgetBytes:             ReviewPacketDiffBudgetBytes,
		ReviewPacketDiffFileBudgetBytes:         ReviewPacketDiffFileBudgetBytes,
		ReviewPacketDocumentationBodyFileBytes:  ReviewPacketDocumentationBodyFileBytes,
		ReviewPacketDocumentationBodyTotalBytes: ReviewPacketDocumentationBodyTotalBytes,
		ReviewPacketDocumentationBodyMaxFiles:   ReviewPacketDocumentationBodyMaxFiles,
		ReviewPacketGeneratedDiffFileBytes:      ReviewPacketGeneratedDiffFileBytes,
		ReviewPacketGeneratedSizeBytes:          ReviewPacketGeneratedSizeBytes,
		ReviewPacketGeneratedPatterns:           ReviewPacketGeneratedPatterns(),
		ReviewPacketIssueBudgetBytes:            ReviewPacketIssueBudgetBytes,
		ReviewPacketRenderedArtifactBudgetBytes: ReviewPacketRenderedArtifactBudgetBytes,
		ReviewPacketRubricBudgetBytes:           ReviewPacketRubricBudgetBytes,
		ReviewPacketSpecBudgetBytes:             ReviewPacketSpecBudgetBytes,
		ReviewPacketTotalPromptBudgetBytes:      ReviewPacketTotalPromptBudgetBytes,
		RenderedArtifactFileBudgetBytes:         RenderedArtifactFileBudgetBytes,
		RenderedArtifactMaxDirectoryFiles:       RenderedArtifactMaxDirectoryFiles,
		RenderedArtifactProducerTimeout:         RenderedArtifactProducerTimeout,
		ProducerFailureLogBudgetBytes:           ProducerFailureLogBudgetBytes,
		ProviderFailureLogBudgetBytes:           ProviderFailureLogBudgetBytes,
		WorktreeLivenessMaxFiles:                WorktreeLivenessMaxFiles,
		CustomLivenessOutputMaxBytes:            CustomLivenessOutputMaxBytes,
		GitHubListLimit:                         GitHubListLimit,
		ClaudeHookTimeoutSeconds:                ClaudeHookTimeoutSeconds,
		HookInputMaxBytes:                       HookInputMaxBytes,
		ConductorHookMaxStateFiles:              ConductorHookMaxStateFiles,
		ConductorHookMaxStateAgeMillis:          ConductorHookMaxStateAgeMillis,
		ConductorHookMaxTextField:               ConductorHookMaxTextField,
		ConductorHookMaxStateRecords:            ConductorHookMaxStateRecords,
		ConductorHookMaxLedgerFiles:             ConductorHookMaxLedgerFiles,
		ConductorHookMaxLedgerBytes:             ConductorHookMaxLedgerBytes,
		ConductorHookRecentLedgerGraceMs:        ConductorHookRecentLedgerGraceMs,
		RunStatusMaxEventBytes:                  RunStatusMaxEventBytes,
		RunStatusMaxEventLineBytes:              RunStatusMaxEventLineBytes,
		RunStatusMaxRecordBytes:                 RunStatusMaxRecordBytes,
		RunStatusMaxDirectoryEntries:            RunStatusMaxDirectoryEntries,
		RunStatusMaxFutureSkew:                  RunStatusMaxFutureSkew,
		RelayGateMaxRecordSize:                  RelayGateMaxRecordSize,
		StateBranchDefaultBranch:                StateBranchDefaultBranch,
		StateBranchDefaultRemote:                StateBranchDefaultRemote,
		StateBranchDefaultTTLSeconds:            StateBranchDefaultTTLSeconds,
		StateBranchLogTailLines:                 StateBranchLogTailLines,
	}
}

// WorkerRetryBackoffSeconds returns a caller-owned retry backoff schedule.
func WorkerRetryBackoffSeconds() []int {
	return append([]int(nil), workerRetryBackoffSeconds...)
}

// ReviewPacketGeneratedPatterns returns caller-owned generated-file patterns.
func ReviewPacketGeneratedPatterns() []string {
	return append([]string(nil), reviewPacketGeneratedPatterns...)
}
