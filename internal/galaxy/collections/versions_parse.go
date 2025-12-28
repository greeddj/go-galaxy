package collections

import "github.com/greeddj/go-galaxy/internal/galaxy/helpers"

// parseVersionsPayload extracts version list and total count from payload.
func parseVersionsPayload(payload map[string]any) ([]string, int, error) {
	if payload == nil {
		return nil, 0, helpers.ErrVersionsPayloadEmpty
	}

	if data, ok := payload["data"].([]any); ok {
		versions := make([]string, 0, len(data))
		for _, item := range data {
			if version := extractVersionField(item); version != "" {
				versions = append(versions, version)
			}
		}
		return versions, parseCountFromMeta(payload["meta"]), nil
	}

	if data, ok := payload["results"].([]any); ok {
		versions := make([]string, 0, len(data))
		for _, item := range data {
			if version := extractVersionField(item); version != "" {
				versions = append(versions, version)
			}
		}
		return versions, parseCount(payload["count"]), nil
	}

	return nil, 0, helpers.ErrVersionsPayloadUnsupported
}

// extractVersionField returns the version field from an item.
func extractVersionField(item any) string {
	if m, ok := item.(map[string]any); ok {
		if value, ok := m["version"].(string); ok {
			return value
		}
	}
	return ""
}

// parseCountFromMeta extracts the count from meta.
func parseCountFromMeta(meta any) int {
	if metaMap, ok := meta.(map[string]any); ok {
		return parseCount(metaMap["count"])
	}
	return 0
}

// parseCount converts numeric count fields to int.
func parseCount(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case float32:
		return int(v)
	default:
		return 0
	}
}
