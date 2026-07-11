package delivery

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"
)

func CanonicalJSON(v any) ([]byte, error) {
	normalized, err := normalizeJSONValue(v)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := writeCanonicalJSON(&out, normalized); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func CanonicalJSONBytes(data []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, typed(ErrInvalidRecordCode, "decode canonical JSON input: %v", err)
	}
	if decoder.More() {
		return nil, typed(ErrInvalidRecordCode, "canonical JSON input contains multiple values")
	}
	return CanonicalJSON(value)
}

func DigestCanonicalJSON(v any) (string, []byte, error) {
	canonical, err := CanonicalJSON(v)
	if err != nil {
		return "", nil, err
	}
	return SHA256Digest(canonical), canonical, nil
}

func SHA256Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func AuthorizationFingerprint(inputFingerprint, policyFingerprint, planFingerprint, maxSideEffectClass string, approvedScope any) (string, []byte, error) {
	scopeBytes, err := CanonicalJSON(approvedScope)
	if err != nil {
		return "", nil, err
	}
	payload := []byte("loopcoder.authorization.v1\n" +
		strings.TrimSpace(inputFingerprint) + "\n" +
		strings.TrimSpace(policyFingerprint) + "\n" +
		strings.TrimSpace(planFingerprint) + "\n" +
		strings.TrimSpace(maxSideEffectClass) + "\n" +
		string(scopeBytes))
	return SHA256Digest(payload), payload, nil
}

func NormalizeRepoPath(value string) (string, error) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" {
		return "", nil
	}
	if strings.HasPrefix(value, "/") || strings.Contains(value, ":") {
		return "", typed(ErrInvalidRecordCode, "repository path %q must be relative", value)
	}
	clean := path.Clean(value)
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return "", typed(ErrInvalidRecordCode, "repository path %q escapes project root", value)
	}
	return clean, nil
}

func CanonicalTimestamp(t time.Time) string {
	t = t.UTC()
	return t.Format(time.RFC3339Nano)
}

func NormalizeTimestamp(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", typed(ErrInvalidRecordCode, "invalid timestamp %q", value)
	}
	return CanonicalTimestamp(parsed), nil
}

func normalizeJSONValue(v any) (any, error) {
	switch value := v.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		out := make(map[string]any, len(value))
		for k, child := range value {
			normalizedKey := norm.NFC.String(k)
			normalizedChild, err := normalizeJSONValue(child)
			if err != nil {
				return nil, err
			}
			out[normalizedKey] = normalizedChild
		}
		return out, nil
	case map[string]string:
		out := make(map[string]any, len(value))
		for k, child := range value {
			out[norm.NFC.String(k)] = norm.NFC.String(child)
		}
		return out, nil
	case []any:
		out := make([]any, len(value))
		for i, child := range value {
			normalizedChild, err := normalizeJSONValue(child)
			if err != nil {
				return nil, err
			}
			out[i] = normalizedChild
		}
		return out, nil
	case []string:
		out := make([]any, len(value))
		for i, child := range value {
			out[i] = norm.NFC.String(child)
		}
		return out, nil
	case string:
		return norm.NFC.String(value), nil
	case bool:
		return value, nil
	case int:
		return int64(value), nil
	case int8:
		return int64(value), nil
	case int16:
		return int64(value), nil
	case int32:
		return int64(value), nil
	case int64:
		return value, nil
	case uint:
		return uint64(value), nil
	case uint8:
		return uint64(value), nil
	case uint16:
		return uint64(value), nil
	case uint32:
		return uint64(value), nil
	case uint64:
		return value, nil
	case json.Number:
		text := value.String()
		if strings.ContainsAny(text, ".eE") {
			return nil, typed(ErrInvalidRecordCode, "floats are not allowed in fingerprint inputs")
		}
		parsed, err := strconv.ParseInt(text, 10, 64)
		if err == nil {
			return parsed, nil
		}
		unsigned, uintErr := strconv.ParseUint(text, 10, 64)
		if uintErr != nil {
			return nil, typed(ErrInvalidRecordCode, "invalid integer %q", text)
		}
		return unsigned, nil
	case float32, float64:
		return nil, typed(ErrInvalidRecordCode, "floats are not allowed in fingerprint inputs")
	default:
		data, err := json.Marshal(value)
		if err != nil {
			return nil, typed(ErrInvalidRecordCode, "marshal value for canonical JSON: %v", err)
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		var decoded any
		if err := decoder.Decode(&decoded); err != nil {
			return nil, typed(ErrInvalidRecordCode, "decode value for canonical JSON: %v", err)
		}
		return normalizeJSONValue(decoded)
	}
}

func writeCanonicalJSON(out *bytes.Buffer, v any) error {
	switch value := v.(type) {
	case nil:
		out.WriteString("null")
	case string:
		encoded, _ := json.Marshal(value)
		out.Write(encoded)
	case bool:
		if value {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case int64:
		out.WriteString(strconv.FormatInt(value, 10))
	case uint64:
		out.WriteString(strconv.FormatUint(value, 10))
	case map[string]any:
		keys := make([]string, 0, len(value))
		for k := range value {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			encodedKey, _ := json.Marshal(key)
			out.Write(encodedKey)
			out.WriteByte(':')
			if err := writeCanonicalJSON(out, value[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	case []any:
		out.WriteByte('[')
		for i, child := range value {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := writeCanonicalJSON(out, child); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	default:
		return fmt.Errorf("unsupported canonical JSON value %T", value)
	}
	return nil
}
