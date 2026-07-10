package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/tchori-labs/tchori/internal/diag"
)

// Ref is a whole-string reference ${type.name.attr} found inside a raw
// resource or provider config.
type Ref struct {
	Address string // "null_resource.demo"
	Attr    string // "id" or dotted path "triggers.foo"
}

// refPattern matches a string that is EXACTLY one reference — no
// interpolation inside larger strings. Group 1 is the resource address
// ("type.name"), group 2 the (possibly dotted) attribute path.
var refPattern = regexp.MustCompile(`^\$\{([a-z][a-z0-9_]*\.[a-z][a-z0-9_-]*)\.([a-zA-Z0-9_.-]+)\}$`)

// ParseRef reports whether s is a whole-string ${type.name.attr} reference
// and, if so, parses it. It is the engine's single reference grammar:
// ExtractRefs and Task 7's Compose both detect references through it, so a
// string is a reference iff it creates a dependency-graph edge.
func ParseRef(s string) (Ref, bool) {
	m := refPattern.FindStringSubmatch(s)
	if m == nil {
		return Ref{}, false
	}
	return Ref{Address: m[1], Attr: m[2]}, true
}

// ExtractRefs walks a raw config map and returns all whole-string
// ${type.name.attr} references, deduplicated, in deterministic order.
func ExtractRefs(cfg map[string]any) []Ref {
	var refs []Ref
	walkRefs(cfg, &refs)
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Address != refs[j].Address {
			return refs[i].Address < refs[j].Address
		}
		return refs[i].Attr < refs[j].Attr
	})
	// Dedupe preserving first-seen order after sorting: drop adjacent
	// duplicates so the result is deterministic regardless of map order.
	var out []Ref
	for i, r := range refs {
		if i == 0 || r != refs[i-1] {
			out = append(out, r)
		}
	}
	return out
}

// walkRefs recurses through the JSON-shaped value collecting references
// from every string it reaches. Non-container, non-string values are
// ignored.
func walkRefs(v any, refs *[]Ref) {
	switch val := v.(type) {
	case string:
		if r, ok := ParseRef(val); ok {
			*refs = append(*refs, r)
		}
	case map[string]any:
		for _, item := range val {
			walkRefs(item, refs)
		}
	case []any:
		for _, item := range val {
			walkRefs(item, refs)
		}
	}
}

// Order returns resource addresses topologically sorted by reference
// edges; a cycle yields an error diagnostic naming the cycle path.
func (c *Config) Order() ([]string, diag.Diagnostics) {
	var diags diag.Diagnostics

	addrs := make([]string, 0, len(c.Resources))
	for addr := range c.Resources {
		addrs = append(addrs, addr)
	}
	sort.Strings(addrs)

	// Build the edge sets. deps[a] lists the addresses a references
	// (sorted, because ExtractRefs returns refs sorted by Address);
	// dependents is the reverse adjacency used by Kahn's algorithm.
	deps := make(map[string][]string, len(addrs))
	dependents := make(map[string][]string, len(addrs))
	indegree := make(map[string]int, len(addrs))

	for _, addr := range addrs {
		seen := make(map[string]bool)
		for _, ref := range ExtractRefs(c.Resources[addr].Config) {
			if _, known := c.Resources[ref.Address]; !known {
				diags = append(diags, diag.Errorf(addr,
					"reference to undeclared resource",
					fmt.Sprintf("%s references ${%s.%s}, but resource %q is not declared",
						addr, ref.Address, ref.Attr, ref.Address)))
				continue
			}
			if seen[ref.Address] {
				continue
			}
			seen[ref.Address] = true
			deps[addr] = append(deps[addr], ref.Address)
			dependents[ref.Address] = append(dependents[ref.Address], addr)
			indegree[addr]++
		}
	}
	if diags.HasErrors() {
		return nil, diags
	}

	// Kahn's algorithm; the ready set is sorted before every pop so ties
	// break deterministically (lexical address order).
	var ready []string
	for _, addr := range addrs {
		if indegree[addr] == 0 {
			ready = append(ready, addr)
		}
	}
	order := make([]string, 0, len(addrs))
	for len(ready) > 0 {
		sort.Strings(ready)
		next := ready[0]
		ready = ready[1:]
		order = append(order, next)
		for _, dep := range dependents[next] {
			indegree[dep]--
			if indegree[dep] == 0 {
				ready = append(ready, dep)
			}
		}
	}
	if len(order) == len(addrs) {
		return order, diags
	}

	// Cycle. Every unordered node still has indegree > 0, i.e. at least
	// one of its dependencies is also unordered, so walking least-first
	// through unordered dependencies from the smallest unordered address
	// must revisit a node — the revisit closes the cycle.
	remaining := make(map[string]bool)
	start := ""
	for _, addr := range addrs {
		if indegree[addr] > 0 {
			remaining[addr] = true
			if start == "" {
				start = addr
			}
		}
	}
	index := make(map[string]int)
	var path []string
	cur := start
	for {
		if i, visited := index[cur]; visited {
			cycle := append(path[i:], cur)
			diags = append(diags, diag.Errorf(cycle[0],
				"reference cycle", strings.Join(cycle, " -> ")))
			return nil, diags
		}
		index[cur] = len(path)
		path = append(path, cur)
		for _, d := range deps[cur] {
			if remaining[d] {
				cur = d
				break
			}
		}
	}
}
