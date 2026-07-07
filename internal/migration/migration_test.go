package migration

import "testing"

func TestReporterRenameEntriesContainSliceOneSurfaces(t *testing.T) {
	want := map[string]string{
		LegacyReporterScopeEnv:    ReporterScopeEnv,
		LegacyReporterStateDirEnv: ReporterStateDirEnv,
		LegacyReporterHookCommand: ReporterHookCommand,
		LegacyReportConfigRoot:    ReportConfigRoot,
		LegacyReportConfigChannel: ReportConfigChannel,
	}

	for legacy, current := range want {
		entry, ok := ReporterRenameEntry(legacy)
		if !ok {
			t.Fatalf("ReporterRenameEntry(%q) missing", legacy)
		}
		if entry.Current != current {
			t.Fatalf("ReporterRenameEntry(%q).Current = %q, want %q", legacy, entry.Current, current)
		}
		if entry.FixCommand == "" {
			t.Fatalf("ReporterRenameEntry(%q).FixCommand is empty", legacy)
		}
	}
}

func TestResolveEnvAcceptsLegacyAndPrefersCurrent(t *testing.T) {
	tests := []struct {
		name         string
		env          map[string]string
		wantValue    string
		wantDiag     bool
		wantConflict bool
	}{
		{
			name: "current only",
			env: map[string]string{
				ReporterScopeEnv: "always",
			},
			wantValue: "always",
		},
		{
			name: "legacy only",
			env: map[string]string{
				LegacyReporterScopeEnv: "auto",
			},
			wantValue: "auto",
			wantDiag:  true,
		},
		{
			name: "current wins conflict",
			env: map[string]string{
				ReporterScopeEnv:       "off",
				LegacyReporterScopeEnv: "always",
			},
			wantValue:    "off",
			wantDiag:     true,
			wantConflict: true,
		},
		{
			name: "current wins same value",
			env: map[string]string{
				ReporterScopeEnv:       "auto",
				LegacyReporterScopeEnv: "auto",
			},
			wantValue: "auto",
			wantDiag:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, diagnostics := ResolveReporterScopeEnv(func(key string) string {
				return tt.env[key]
			})
			if got != tt.wantValue {
				t.Fatalf("ResolveReporterScopeEnv value = %q, want %q", got, tt.wantValue)
			}
			if (len(diagnostics) > 0) != tt.wantDiag {
				t.Fatalf("diagnostics = %#v, want present=%v", diagnostics, tt.wantDiag)
			}
			if len(diagnostics) > 0 && diagnostics[0].Conflict != tt.wantConflict {
				t.Fatalf("diagnostic conflict = %v, want %v", diagnostics[0].Conflict, tt.wantConflict)
			}
		})
	}
}
