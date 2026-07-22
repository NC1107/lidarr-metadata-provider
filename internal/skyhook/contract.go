package skyhook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
)

// ContractDiff strict-decodes raw into target, re-marshals it, and returns
// the semantic differences between the two JSON trees: keys upstream sent
// that the structs drop, keys the structs would add, and casing, type or
// value drift. Key order is ignored. An empty result means target's type
// reproduces raw exactly. The error return is reserved for input that cannot
// be checked at all (malformed JSON); unknown fields and shape mismatches are
// findings, not errors.
func ContractDiff(raw []byte, target any) ([]string, error) {
	var want any
	if err := json.Unmarshal(raw, &want); err != nil {
		return nil, fmt.Errorf("input is not valid JSON: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		// Decoding stops at the first offending field, so this is the first
		// drift, not necessarily the only one.
		return []string{fmt.Sprintf("strict decode into %T: %v", target, err)}, nil
	}
	emitted, err := json.Marshal(target)
	if err != nil {
		return nil, err
	}
	var got any
	if err := json.Unmarshal(emitted, &got); err != nil {
		return nil, err
	}
	return diffTree("$", want, got, nil), nil
}

// diffTree reports semantic JSON differences as JSONPath-ish strings. Key
// order is irrelevant by construction (maps); everything else - missing keys,
// extra keys, type changes, value changes - is reported.
func diffTree(path string, want, got any, acc []string) []string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return append(acc, fmt.Sprintf("%s: fixture has object, emitted %T", path, got))
		}
		keys := map[string]bool{}
		for k := range w {
			keys[k] = true
		}
		for k := range g {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			wv, inW := w[k]
			gv, inG := g[k]
			switch {
			case !inW:
				acc = append(acc, fmt.Sprintf("%s.%s: emitted key absent from fixture", path, k))
			case !inG:
				acc = append(acc, fmt.Sprintf("%s.%s: fixture key missing from emission", path, k))
			default:
				acc = diffTree(path+"."+k, wv, gv, acc)
			}
		}
		return acc
	case []any:
		g, ok := got.([]any)
		if !ok {
			return append(acc, fmt.Sprintf("%s: fixture has array, emitted %T", path, got))
		}
		if len(w) != len(g) {
			return append(acc, fmt.Sprintf("%s: fixture has %d elements, emitted %d", path, len(w), len(g)))
		}
		for i := range w {
			acc = diffTree(fmt.Sprintf("%s[%d]", path, i), w[i], g[i], acc)
		}
		return acc
	default:
		if !reflect.DeepEqual(want, got) {
			acc = append(acc, fmt.Sprintf("%s: fixture %v (%T), emitted %v (%T)", path, want, want, got, got))
		}
		return acc
	}
}
