package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"reflect"

	"github.com/jasonhnd/loopcoder/internal/migration"
	"gopkg.in/yaml.v3"
)

type DeliveryMigrationResult struct {
	Data        []byte
	Diagnostics []migration.Diagnostic
	Changed     bool
}

func ParseWithDiagnostics(data []byte) (Config, []migration.Diagnostic, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		cfg := Default()
		if err := validateParsedConfig(cfg); err != nil {
			return Config{}, nil, err
		}
		return cfg, nil, nil
	}

	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return Config{}, nil, fmt.Errorf("parse delivery config: %w", err)
	}
	diagnostics, _ := applyDeliveryMigration(&node, false)

	cfg := Default()
	if err := node.Decode(&cfg); err != nil {
		return Config{}, diagnostics, fmt.Errorf("parse delivery config: %w", err)
	}
	if err := validateParsedConfig(cfg); err != nil {
		return Config{}, diagnostics, err
	}
	return cfg, diagnostics, nil
}

func MigrateDeliveryYAML(data []byte) (DeliveryMigrationResult, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return DeliveryMigrationResult{Data: append([]byte(nil), data...)}, nil
	}

	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return DeliveryMigrationResult{}, fmt.Errorf("parse delivery config: %w", err)
	}
	diagnostics, changed := applyDeliveryMigration(&node, true)
	if !changed {
		return DeliveryMigrationResult{
			Data:        append([]byte(nil), data...),
			Diagnostics: diagnostics,
		}, nil
	}

	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(&node); err != nil {
		_ = encoder.Close()
		return DeliveryMigrationResult{}, fmt.Errorf("render migrated delivery config: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return DeliveryMigrationResult{}, fmt.Errorf("render migrated delivery config: %w", err)
	}
	return DeliveryMigrationResult{
		Data:        out.Bytes(),
		Diagnostics: diagnostics,
		Changed:     true,
	}, nil
}

func applyDeliveryMigration(node *yaml.Node, rewrite bool) ([]migration.Diagnostic, bool) {
	root := deliveryMapping(node)
	if root == nil {
		return nil, false
	}

	oldKeyIndex, oldValueIndex, hasOld := mappingField(root, migration.LegacyReportConfigRoot)
	if !hasOld {
		return nil, false
	}
	_, newValueIndex, hasNew := mappingField(root, migration.ReportConfigRoot)

	var diagnostics []migration.Diagnostic
	rootEntry := mustMigrationEntry(migration.LegacyReportConfigRoot)
	rootConflict := hasNew && !yamlNodesEqual(root.Content[oldValueIndex], root.Content[newValueIndex])
	diagnostics = append(diagnostics, migration.NewDiagnostic(rootEntry, rootConflict, ""))

	oldReport := root.Content[oldValueIndex]
	if _, oldChannelValue, ok := mappingField(oldReport, "channel"); ok {
		channelEntry := mustMigrationEntry(migration.LegacyReportConfigChannel)
		channelConflict := false
		if hasNew {
			if newReport := root.Content[newValueIndex]; newReport != nil {
				if _, newChannelValue, hasNewChannel := mappingField(newReport, "channel"); hasNewChannel {
					channelConflict = !yamlNodesEqual(oldReport.Content[oldChannelValue], newReport.Content[newChannelValue])
				}
			}
		}
		diagnostics = append(diagnostics, migration.NewDiagnostic(channelEntry, channelConflict, ""))
	}

	if hasNew {
		if rewrite {
			removeMappingFieldAt(root, oldKeyIndex)
			return diagnostics, true
		}
		return diagnostics, false
	}

	root.Content[oldKeyIndex].Value = migration.ReportConfigRoot
	return diagnostics, true
}

func deliveryMapping(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	current := node
	if current.Kind == yaml.DocumentNode {
		if len(current.Content) == 0 {
			return nil
		}
		current = current.Content[0]
	}
	if current.Kind != yaml.MappingNode {
		return nil
	}
	return current
}

func mappingField(node *yaml.Node, key string) (int, int, bool) {
	if node == nil || node.Kind != yaml.MappingNode {
		return 0, 0, false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Kind == yaml.ScalarNode && node.Content[i].Value == key {
			return i, i + 1, true
		}
	}
	return 0, 0, false
}

func removeMappingFieldAt(node *yaml.Node, keyIndex int) {
	if node == nil || keyIndex < 0 || keyIndex+1 >= len(node.Content) {
		return
	}
	node.Content = append(node.Content[:keyIndex], node.Content[keyIndex+2:]...)
}

func yamlNodesEqual(a, b *yaml.Node) bool {
	if a == nil || b == nil {
		return a == b
	}
	var leftValue any
	var rightValue any
	if leftErr := a.Decode(&leftValue); leftErr == nil {
		if rightErr := b.Decode(&rightValue); rightErr == nil {
			return reflect.DeepEqual(leftValue, rightValue)
		}
	}
	left, leftErr := yaml.Marshal(a)
	right, rightErr := yaml.Marshal(b)
	return leftErr == nil && rightErr == nil && bytes.Equal(bytes.TrimSpace(left), bytes.TrimSpace(right))
}

func mustMigrationEntry(legacy string) migration.Entry {
	entry, ok := migration.ReporterRenameEntry(legacy)
	if !ok {
		panic("missing migration entry for " + legacy)
	}
	return entry
}

func warnMigrationDiagnostics(w io.Writer, diagnostics []migration.Diagnostic) {
	if len(diagnostics) == 0 {
		return
	}
	if w == nil {
		w = os.Stderr
	}
	for _, diagnostic := range diagnostics {
		fmt.Fprintf(w, "[loopcoder] warning: %s\n", diagnostic.String())
	}
}
