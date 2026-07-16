package progress

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"
)

func canonicalJSON(v any) ([]byte, error) {
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

func digestCanonicalJSON(v any) (string, []byte, error) {
	canonical, err := canonicalJSON(v)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), canonical, nil
}

func canonicalTimestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func normalizeJSONValue(v any) (any, error) {
	switch value := v.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		out := make(map[string]any, len(value))
		for k, child := range value {
			normalizedChild, err := normalizeJSONValue(child)
			if err != nil {
				return nil, err
			}
			out[norm.NFC.String(k)] = normalizedChild
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
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("marshal canonical string: %w", err)
		}
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
			encodedKey, err := json.Marshal(key)
			if err != nil {
				return fmt.Errorf("marshal canonical key: %w", err)
			}
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
		return typed(ErrInvalidRecordCode, "unsupported canonical JSON value %T", value)
	}
	return nil
}
