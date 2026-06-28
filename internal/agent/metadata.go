package agent

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/attestation"
)

type resultMetadata struct {
	Model  string
	Effort string
	Usage  attestation.Usage
}

func resultWithTiming(exitCode int, summary string, metadata resultMetadata, started, ended time.Time) Result {
	durationMS := ended.Sub(started).Milliseconds()
	if durationMS < 0 {
		durationMS = 0
	}
	return Result{
		ExitCode:   exitCode,
		Summary:    summary,
		Model:      metadata.Model,
		Effort:     metadata.Effort,
		Usage:      metadata.Usage,
		StartedAt:  started.UTC().Format(time.RFC3339Nano),
		EndedAt:    ended.UTC().Format(time.RFC3339Nano),
		DurationMS: durationMS,
	}
}

func decodeJSONMap(data []byte) (map[string]any, bool) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, false
	}

	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, false
	}
	root, ok := decoded.(map[string]any)
	return root, ok
}

func objectFromMap(obj map[string]any, names ...string) (map[string]any, bool) {
	for _, name := range names {
		value, ok := obj[name]
		if !ok {
			continue
		}
		child, ok := value.(map[string]any)
		if ok {
			return child, true
		}
	}
	return nil, false
}

func stringFromMap(obj map[string]any, names ...string) string {
	for _, name := range names {
		if value, ok := obj[name]; ok {
			if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
	}
	return ""
}

func modelFromKeyedMap(obj map[string]any) string {
	keys := make([]string, 0, len(obj))
	for key := range obj {
		if strings.TrimSpace(key) != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ""
	}
	return strings.TrimSpace(keys[0])
}

func usageFromFields(obj map[string]any) attestation.Usage {
	var usage attestation.Usage
	if input, ok := int64FromMap(obj, "input_tokens", "inputTokens", "inputTokenCount", "input_token_count", "prompt_tokens", "promptTokens", "promptTokenCount", "prompt_token_count", "prompt", "input"); ok {
		usage.InputTokens = int64Ptr(input)
	}
	if output, ok := int64FromMap(obj, "output_tokens", "outputTokens", "outputTokenCount", "output_token_count", "completion_tokens", "completionTokens", "candidate_tokens", "candidateTokens", "candidatesTokenCount", "candidates_token_count", "completion", "candidates", "output"); ok {
		usage.OutputTokens = int64Ptr(output)
	}
	if total, ok := int64FromMap(obj, "total_tokens", "totalTokens", "totalTokenCount", "total_token_count", "tokens_used", "tokensUsed", "tokenCount", "token_count", "total"); ok {
		usage.TotalTokens = int64Ptr(total)
	}
	return usage
}

func firstUsageInObject(obj map[string]any, depth int) attestation.Usage {
	if depth < 0 {
		return attestation.Usage{}
	}
	if usage := usageFromFields(obj); hasUsage(usage) {
		return usage
	}

	preferred := []string{"usage", "usageMetadata", "usage_metadata", "tokenUsage", "token_usage", "tokens", "stats", "models"}
	for _, name := range preferred {
		child, ok := objectFromMap(obj, name)
		if !ok {
			continue
		}
		if usage := firstUsageInObject(child, depth-1); hasUsage(usage) {
			return usage
		}
	}

	keys := make([]string, 0, len(obj))
	for key := range obj {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		child, ok := obj[key].(map[string]any)
		if !ok {
			continue
		}
		if usage := firstUsageInObject(child, depth-1); hasUsage(usage) {
			return usage
		}
	}
	return attestation.Usage{}
}

func hasUsage(usage attestation.Usage) bool {
	return usage.InputTokens != nil || usage.OutputTokens != nil || usage.TotalTokens != nil
}

func int64FromMap(obj map[string]any, names ...string) (int64, bool) {
	for _, name := range names {
		value, ok := obj[name]
		if !ok {
			continue
		}
		if parsed, ok := int64Value(value); ok {
			return parsed, true
		}
	}
	return 0, false
}

func int64Value(value any) (int64, bool) {
	switch v := value.(type) {
	case json.Number:
		if parsed, err := v.Int64(); err == nil {
			return parsed, true
		}
	case float64:
		parsed := int64(v)
		if float64(parsed) == v {
			return parsed, true
		}
	case int:
		return int64(v), true
	case int64:
		return v, true
	case string:
		return parseInt64Text(v)
	}
	return 0, false
}

func parseInt64Text(text string) (int64, bool) {
	normalized := strings.NewReplacer(",", "", "_", "", " ", "").Replace(strings.TrimSpace(text))
	if normalized == "" {
		return 0, false
	}
	parsed, err := strconv.ParseInt(normalized, 10, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

func int64Ptr(value int64) *int64 {
	copied := value
	return &copied
}
