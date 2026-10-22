package resolve

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
)

func decodeObject(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("unexpected data after the JSON object")
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("must be a JSON object")
	}
	return obj, nil
}

func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

// mergeObjects deep-merges src into dst: objects merge, arrays union with
// dst's elements first, and a scalar present in both must be equal.
func mergeObjects(dst, src map[string]any, prefix string) error {
	for _, k := range sortedKeys(src) {
		sv := src[k]
		key := prefix + k
		dv, exists := dst[k]
		if !exists {
			dst[k] = sv
			continue
		}
		switch d := dv.(type) {
		case map[string]any:
			s, ok := sv.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: type conflict with parent (object vs %T)", key, sv)
			}
			if err := mergeObjects(d, s, key+"."); err != nil {
				return err
			}
		case []any:
			s, ok := sv.([]any)
			if !ok {
				return fmt.Errorf("%s: type conflict with parent (array vs %T)", key, sv)
			}
			for _, e := range s {
				if !containsValue(d, e) {
					d = append(d, e)
				}
			}
			dst[k] = d
		default:
			if !reflect.DeepEqual(dv, sv) {
				return fmt.Errorf("%s: scalar already set by a parent spec to a different value", key)
			}
		}
	}
	return nil
}

func containsValue(list []any, v any) bool {
	for _, e := range list {
		if reflect.DeepEqual(e, v) {
			return true
		}
	}
	return false
}
