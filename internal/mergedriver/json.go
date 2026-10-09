package mergedriver

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/tailscale/hujson"
)

// MergeJSON merges JSON or JSONC (comments, trailing commas, as in VSCode's
// settings.json) key by key. Theirs' changes are applied to ours as a JSON
// patch through hujson, so ours keeps its comments and formatting.
// Arrays are treated as single values.
func MergeJSON(base, ours, theirs []byte) ([]byte, []string, error) {
	o, err := decodeJSONC(base)
	if err != nil {
		return nil, nil, fmt.Errorf("base: %w", err)
	}
	a, err := decodeJSONC(ours)
	if err != nil {
		return nil, nil, fmt.Errorf("ours: %w", err)
	}
	b, err := decodeJSONC(theirs)
	if err != nil {
		return nil, nil, fmt.Errorf("theirs: %w", err)
	}
	var ops []patchOp
	var conflicts []string
	diff3(o, a, b, "", &ops, &conflicts)
	if len(conflicts) > 0 {
		return nil, conflicts, nil
	}
	v, err := hujson.Parse(ours)
	if err != nil {
		return nil, nil, err
	}
	if len(ops) > 0 {
		p, _ := json.Marshal(ops)
		if err := v.Patch(p); err != nil {
			return nil, nil, err
		}
	}
	return v.Pack(), nil, nil
}

type patchOp struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value,omitempty"`
}

func decodeJSONC(data []byte) (map[string]any, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]any{}, nil
	}
	std, err := hujson.Standardize(data)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(std, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// diff3 collects the patch that carries theirs' changes (relative to base)
// into ours, and the keys both sides changed differently.
func diff3(base, ours, theirs map[string]any, ptr string, ops *[]patchOp, conflicts *[]string) {
	keys := map[string]bool{}
	for k := range base {
		keys[k] = true
	}
	for k := range theirs {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		bv, bok := base[k]
		tv, tok := theirs[k]
		ov, ook := ours[k]
		if bok == tok && reflect.DeepEqual(bv, tv) {
			continue // theirs didn't touch it
		}
		p := ptr + "/" + escapePointer(k)
		if ook == tok && reflect.DeepEqual(ov, tv) {
			continue // both made the same change
		}
		om, oIsMap := ov.(map[string]any)
		tm, tIsMap := tv.(map[string]any)
		if oIsMap && tIsMap {
			bm, _ := bv.(map[string]any)
			if bm == nil {
				bm = map[string]any{}
			}
			diff3(bm, om, tm, p, ops, conflicts)
			continue
		}
		if ook != bok || !reflect.DeepEqual(ov, bv) {
			*conflicts = append(*conflicts, strings.TrimPrefix(p, "/"))
			continue
		}
		switch {
		case !tok:
			*ops = append(*ops, patchOp{Op: "remove", Path: p})
		case !ook:
			*ops = append(*ops, patchOp{Op: "add", Path: p, Value: raw(tv)})
		default:
			*ops = append(*ops, patchOp{Op: "replace", Path: p, Value: raw(tv)})
		}
	}
}

func raw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func escapePointer(k string) string {
	return strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1")
}

// JSONKeys lists the leaf keys that differ between two JSON(C) documents, for
// commit summaries. Nested keys are joined with "/".
func JSONKeys(before, after []byte) ([]string, map[string]any, error) {
	a, err := decodeJSONC(before)
	if err != nil {
		return nil, nil, err
	}
	b, err := decodeJSONC(after)
	if err != nil {
		return nil, nil, err
	}
	values := map[string]any{}
	var keys []string
	var walk func(a, b map[string]any, prefix string)
	walk = func(a, b map[string]any, prefix string) {
		all := map[string]bool{}
		for k := range a {
			all[k] = true
		}
		for k := range b {
			all[k] = true
		}
		for k := range all {
			av, aok := a[k]
			bv, bok := b[k]
			if aok == bok && reflect.DeepEqual(av, bv) {
				continue
			}
			am, aIsMap := av.(map[string]any)
			bm, bIsMap := bv.(map[string]any)
			if aIsMap && bIsMap {
				walk(am, bm, prefix+k+"/")
				continue
			}
			keys = append(keys, prefix+k)
			if bok {
				values[prefix+k] = bv
			}
		}
	}
	walk(a, b, "")
	sort.Strings(keys)
	return keys, values, nil
}
