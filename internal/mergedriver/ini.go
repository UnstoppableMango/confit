package mergedriver

import (
	"fmt"
	"sort"
	"strings"
)

// INI is a parsed dconf-style keyfile: section -> key -> raw value.
type INI map[string]map[string]string

// ParseINI parses `dconf dump` output. Values are kept verbatim (GVariant
// text). Blank lines and comments are dropped.
func ParseINI(data []byte) (INI, error) {
	ini := INI{}
	var cur string
	for n, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";"):
		case strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]"):
			cur = t[1 : len(t)-1]
			if ini[cur] == nil {
				ini[cur] = map[string]string{}
			}
		default:
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				return nil, fmt.Errorf("line %d: expected key=value", n+1)
			}
			if ini[cur] == nil {
				ini[cur] = map[string]string{}
			}
			ini[cur][strings.TrimSpace(k)] = v
		}
	}
	return ini, nil
}

// Bytes serializes with sections and keys sorted, the stable form confit stores.
func (ini INI) Bytes() []byte {
	var b strings.Builder
	sections := make([]string, 0, len(ini))
	for s := range ini {
		sections = append(sections, s)
	}
	sort.Strings(sections)
	for i, s := range sections {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "[%s]\n", s)
		keys := make([]string, 0, len(ini[s]))
		for k := range ini[s] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "%s=%s\n", k, ini[s][k])
		}
	}
	return []byte(b.String())
}

// MergeINI merges per key. The result is in the stable sorted form.
func MergeINI(base, ours, theirs []byte) ([]byte, []string, error) {
	o, err := ParseINI(base)
	if err != nil {
		return nil, nil, fmt.Errorf("base: %w", err)
	}
	a, err := ParseINI(ours)
	if err != nil {
		return nil, nil, fmt.Errorf("ours: %w", err)
	}
	b, err := ParseINI(theirs)
	if err != nil {
		return nil, nil, fmt.Errorf("theirs: %w", err)
	}
	out := INI{}
	var conflicts []string
	for _, k := range iniKeys(o, a, b) {
		bv, bok := o.get(k)
		ov, ook := a.get(k)
		tv, tok := b.get(k)
		var v string
		var keep bool
		switch {
		case ook == tok && ov == tv:
			v, keep = ov, ook
		case ook == bok && ov == bv:
			v, keep = tv, tok
		case tok == bok && tv == bv:
			v, keep = ov, ook
		default:
			conflicts = append(conflicts, k[0]+"/"+k[1])
			continue
		}
		if keep {
			if out[k[0]] == nil {
				out[k[0]] = map[string]string{}
			}
			out[k[0]][k[1]] = v
		}
	}
	if len(conflicts) > 0 {
		return nil, conflicts, nil
	}
	return out.Bytes(), nil, nil
}

func (ini INI) get(k [2]string) (string, bool) {
	v, ok := ini[k[0]][k[1]]
	return v, ok
}

func iniKeys(inis ...INI) [][2]string {
	seen := map[[2]string]bool{}
	var keys [][2]string
	for _, ini := range inis {
		for s, kv := range ini {
			for k := range kv {
				if !seen[[2]string{s, k}] {
					seen[[2]string{s, k}] = true
					keys = append(keys, [2]string{s, k})
				}
			}
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	return keys
}

// INIKeys lists "section/key" entries that differ between two keyfiles.
func INIKeys(before, after []byte) ([]string, map[string]string, error) {
	a, err := ParseINI(before)
	if err != nil {
		return nil, nil, err
	}
	b, err := ParseINI(after)
	if err != nil {
		return nil, nil, err
	}
	var keys []string
	values := map[string]string{}
	for _, k := range iniKeys(a, b) {
		av, aok := a.get(k)
		bv, bok := b.get(k)
		if aok != bok || av != bv {
			keys = append(keys, k[0]+"/"+k[1])
			if bok {
				values[k[0]+"/"+k[1]] = bv
			}
		}
	}
	return keys, values, nil
}
