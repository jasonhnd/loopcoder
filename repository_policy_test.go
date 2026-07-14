package loopcoder

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var requiredV080Contexts = []string{"verify", "test", "race", "security"}

func TestV080WorkflowPolicy(t *testing.T) {
	root := repositoryPolicyRoot(t)
	workflow := loadWorkflowPolicy(t, filepath.Join(root, ".github", "workflows", "ci.yml"))
	checks := loadDeliveryChecks(t, filepath.Join(root, ".delivery.yml"))

	if err := validateV080WorkflowPolicy(workflow, checks); err != nil {
		t.Fatal(err)
	}
}

func TestV080WorkflowPolicyRejectsUnsupportedShapes(t *testing.T) {
	baseWorkflow := workflowPolicy{
		Jobs: map[string]workflowJobPolicy{
			"verify": {
				RunsOn: yamlScalarOrSequence{Values: []string{"macos-15"}},
				Steps:  []workflowStepPolicy{{Name: "Assert darwin/arm64 Go host", Run: darwinARM64AssertionScript()}},
			},
			"test": {
				RunsOn: yamlScalarOrSequence{Values: []string{"macos-15"}},
				Steps:  []workflowStepPolicy{{Name: "Assert darwin/arm64 Go host", Run: darwinARM64AssertionScript()}},
			},
			"race": {
				RunsOn: yamlScalarOrSequence{Values: []string{"macos-15"}},
				Steps:  []workflowStepPolicy{{Name: "Assert darwin/arm64 Go host", Run: darwinARM64AssertionScript()}},
			},
			"security": {
				RunsOn: yamlScalarOrSequence{Values: []string{"macos-15"}},
				Steps:  []workflowStepPolicy{{Name: "Assert darwin/arm64 Go host", Run: darwinARM64AssertionScript()}},
			},
		},
	}
	baseChecks := append([]string(nil), requiredV080Contexts...)

	tests := []struct {
		name    string
		mutate  func(*workflowPolicy, *[]string)
		wantErr string
	}{
		{
			name: "ubuntu runner",
			mutate: func(workflow *workflowPolicy, checks *[]string) {
				job := workflow.Jobs["test"]
				job.RunsOn = yamlScalarOrSequence{Values: []string{"ubuntu-latest"}}
				workflow.Jobs["test"] = job
			},
			wantErr: "unsupported runner",
		},
		{
			name: "windows matrix",
			mutate: func(workflow *workflowPolicy, checks *[]string) {
				job := workflow.Jobs["test"]
				job.Strategy.Matrix = map[string]any{"os": []any{"macos-15", "windows-latest"}}
				workflow.Jobs["test"] = job
			},
			wantErr: "unsupported matrix value",
		},
		{
			name: "macos latest",
			mutate: func(workflow *workflowPolicy, checks *[]string) {
				job := workflow.Jobs["test"]
				job.RunsOn = yamlScalarOrSequence{Values: []string{"macos-latest"}}
				workflow.Jobs["test"] = job
			},
			wantErr: "unsupported runner",
		},
		{
			name: "intel macos",
			mutate: func(workflow *workflowPolicy, checks *[]string) {
				job := workflow.Jobs["test"]
				job.RunsOn = yamlScalarOrSequence{Values: []string{"macos-13"}}
				workflow.Jobs["test"] = job
			},
			wantErr: "unsupported runner",
		},
		{
			name: "missing tuple assertion",
			mutate: func(workflow *workflowPolicy, checks *[]string) {
				job := workflow.Jobs["race"]
				job.Steps = []workflowStepPolicy{{Name: "Go test", Run: "go test ./..."}}
				workflow.Jobs["race"] = job
			},
			wantErr: "does not assert darwin/arm64",
		},
		{
			name: "legacy context",
			mutate: func(workflow *workflowPolicy, checks *[]string) {
				workflow.Jobs["go"] = workflow.Jobs["test"]
			},
			wantErr: "workflow job contexts",
		},
		{
			name: "extra delivery check",
			mutate: func(workflow *workflowPolicy, checks *[]string) {
				*checks = append(*checks, "audit")
			},
			wantErr: "delivery ci.checks",
		},
		{
			name: "missing required context",
			mutate: func(workflow *workflowPolicy, checks *[]string) {
				delete(workflow.Jobs, "security")
			},
			wantErr: "workflow job contexts",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workflow := cloneWorkflowPolicy(baseWorkflow)
			checks := append([]string(nil), baseChecks...)
			tt.mutate(&workflow, &checks)

			err := validateV080WorkflowPolicy(workflow, checks)
			if err == nil {
				t.Fatal("validateV080WorkflowPolicy returned nil error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateV080WorkflowPolicy error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}

type workflowPolicy struct {
	Jobs map[string]workflowJobPolicy `yaml:"jobs"`
}

type workflowJobPolicy struct {
	Name     string                 `yaml:"name"`
	RunsOn   yamlScalarOrSequence   `yaml:"runs-on"`
	Strategy workflowStrategyPolicy `yaml:"strategy"`
	Steps    []workflowStepPolicy   `yaml:"steps"`
}

type workflowStrategyPolicy struct {
	Matrix map[string]any `yaml:"matrix"`
}

type workflowStepPolicy struct {
	Name string `yaml:"name"`
	Run  string `yaml:"run"`
}

type yamlScalarOrSequence struct {
	Values []string
}

func (value *yamlScalarOrSequence) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		value.Values = []string{node.Value}
	case yaml.SequenceNode:
		for _, item := range node.Content {
			if item.Kind != yaml.ScalarNode {
				return fmt.Errorf("runs-on contains non-scalar value")
			}
			value.Values = append(value.Values, item.Value)
		}
	default:
		return fmt.Errorf("runs-on must be a scalar or sequence")
	}
	return nil
}

func validateV080WorkflowPolicy(workflow workflowPolicy, deliveryChecks []string) error {
	if !sameStringSet(deliveryChecks, requiredV080Contexts) {
		return fmt.Errorf("delivery ci.checks = %v, want exactly %v", deliveryChecks, requiredV080Contexts)
	}

	jobIDs := make([]string, 0, len(workflow.Jobs))
	for jobID := range workflow.Jobs {
		jobIDs = append(jobIDs, jobID)
	}
	if !sameStringSet(jobIDs, requiredV080Contexts) {
		return fmt.Errorf("workflow job contexts = %v, want exactly %v", sortedStrings(jobIDs), requiredV080Contexts)
	}

	for _, jobID := range requiredV080Contexts {
		job := workflow.Jobs[jobID]
		if !reflect.DeepEqual(job.RunsOn.Values, []string{"macos-15"}) {
			return fmt.Errorf("%s uses unsupported runner %v; want [macos-15]", jobID, job.RunsOn.Values)
		}
		for _, runner := range job.RunsOn.Values {
			if unsupportedWorkflowValue(runner) {
				return fmt.Errorf("%s uses unsupported runner %q", jobID, runner)
			}
		}
		for _, value := range flattenWorkflowValues(job.Strategy.Matrix) {
			if unsupportedWorkflowValue(value) {
				return fmt.Errorf("%s has unsupported matrix value %q", jobID, value)
			}
		}
		if !jobAssertsDarwinARM64(job) {
			return fmt.Errorf("%s does not assert darwin/arm64 before substantive work", jobID)
		}
	}

	return nil
}

func loadWorkflowPolicy(t *testing.T, path string) workflowPolicy {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read workflow policy %s: %v", path, err)
	}
	var workflow workflowPolicy
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatalf("parse workflow policy %s: %v", path, err)
	}
	return workflow
}

func loadDeliveryChecks(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read delivery config %s: %v", path, err)
	}
	var delivery struct {
		CI struct {
			Checks []string `yaml:"checks"`
		} `yaml:"ci"`
	}
	if err := yaml.Unmarshal(data, &delivery); err != nil {
		t.Fatalf("parse delivery config %s: %v", path, err)
	}
	return delivery.CI.Checks
}

func repositoryPolicyRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repository root")
		}
		dir = parent
	}
}

func jobAssertsDarwinARM64(job workflowJobPolicy) bool {
	for _, step := range job.Steps {
		run := step.Run
		if strings.Contains(run, "go env GOOS") &&
			strings.Contains(run, "go env GOARCH") &&
			strings.Contains(run, "darwin") &&
			strings.Contains(run, "arm64") {
			return true
		}
	}
	return false
}

func unsupportedWorkflowValue(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	return strings.Contains(normalized, "ubuntu") ||
		strings.Contains(normalized, "windows") ||
		strings.Contains(normalized, "linux") ||
		normalized == "macos-latest" ||
		normalized == "macos-13" ||
		strings.Contains(normalized, "amd64") ||
		strings.Contains(normalized, "x64")
}

func flattenWorkflowValues(value any) []string {
	switch typed := value.(type) {
	case nil:
		return nil
	case string:
		return []string{typed}
	case []any:
		var out []string
		for _, item := range typed {
			out = append(out, flattenWorkflowValues(item)...)
		}
		return out
	case map[string]any:
		var out []string
		for _, item := range typed {
			out = append(out, flattenWorkflowValues(item)...)
		}
		return out
	default:
		return []string{fmt.Sprint(typed)}
	}
}

func sameStringSet(got, want []string) bool {
	return reflect.DeepEqual(sortedStrings(got), sortedStrings(want))
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func cloneWorkflowPolicy(workflow workflowPolicy) workflowPolicy {
	clone := workflowPolicy{Jobs: map[string]workflowJobPolicy{}}
	for jobID, job := range workflow.Jobs {
		job.RunsOn.Values = append([]string(nil), job.RunsOn.Values...)
		job.Steps = append([]workflowStepPolicy(nil), job.Steps...)
		job.Strategy.Matrix = cloneWorkflowMap(job.Strategy.Matrix)
		clone.Jobs[jobID] = job
	}
	return clone
}

func cloneWorkflowMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = cloneWorkflowValue(value)
	}
	return out
}

func cloneWorkflowValue(value any) any {
	switch typed := value.(type) {
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, cloneWorkflowValue(item))
		}
		return out
	case map[string]any:
		return cloneWorkflowMap(typed)
	default:
		return typed
	}
}

func darwinARM64AssertionScript() string {
	return "test \"$(go env GOOS)\" = darwin\ntest \"$(go env GOARCH)\" = arm64"
}
