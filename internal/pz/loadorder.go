package pz

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Project Zomboid loads mods in the order the Mods= line gives, so a mod that
// builds on another has to appear after it. Mod authors declare that in
// mod.info with require, loadAfter, loadBefore and incompatibleMods, and the
// community keeps a sorting_rules.txt for mods whose authors did not.
//
// Sorting is therefore a topological sort over those declarations, not a guess.
// Where nothing constrains two mods, the operator's existing order is kept: a
// sort that reshuffles a working list for no reason is worse than no sort.

// OrderIssue is one problem found while sorting.
type OrderIssue struct {
	// Kind is one of: missing, cycle, incompatible, moved.
	Kind string `json:"kind"`
	// Mod is the mod the issue is about.
	Mod string `json:"mod"`
	// Other is the mod it relates to, where relevant.
	Other string `json:"other,omitempty"`
	// Detail is a sentence an operator can act on.
	Detail string `json:"detail"`
}

// OrderReport describes the outcome of a sort.
type OrderReport struct {
	Order   []string     `json:"order"`
	Changed bool         `json:"changed"`
	Issues  []OrderIssue `json:"issues"`
	// Rules counts how many ordering constraints were actually applied, so the
	// interface can say "nothing to go on" rather than implying it did work.
	Rules int `json:"rules"`
}

// SortRules are ordering constraints from outside mod.info.
type SortRules map[string]Mod

// SortLoadOrder returns the mod list reordered so every declared dependency
// comes first. Mods absent from info keep their place; nothing is dropped.
func SortLoadOrder(order []string, info map[string]Mod, extra SortRules) OrderReport {
	report := OrderReport{}

	// Index the requested order, case-insensitively, since mod IDs are written
	// by hand into the ini and casing drifts.
	position := map[string]int{}
	canonical := map[string]string{}
	for i, id := range order {
		key := strings.ToLower(id)
		if _, dup := position[key]; dup {
			report.Issues = append(report.Issues, OrderIssue{
				Kind: "duplicate", Mod: id,
				Detail: id + " appears more than once in the load order.",
			})
			continue
		}
		position[key] = i
		canonical[key] = id
	}

	rulesFor := func(id string) Mod {
		key := strings.ToLower(id)
		merged := info[canonical[key]]
		if merged.ID == "" {
			for k, m := range info {
				if strings.EqualFold(k, id) {
					merged = m
					break
				}
			}
		}
		if e, ok := extra[strings.ToLower(id)]; ok {
			merged.LoadAfter = append(merged.LoadAfter, e.LoadAfter...)
			merged.LoadBefore = append(merged.LoadBefore, e.LoadBefore...)
			merged.Incompatible = append(merged.Incompatible, e.Incompatible...)
			merged.Require = append(merged.Require, e.Require...)
			merged.LoadFirst = merged.LoadFirst || e.LoadFirst
			merged.LoadLast = merged.LoadLast || e.LoadLast
		}
		return merged
	}

	// Build the dependency edges. An edge from A to B means A loads before B.
	edges := map[string]map[string]bool{}
	indegree := map[string]int{}
	for key := range position {
		edges[key] = map[string]bool{}
		indegree[key] = 0
	}
	addEdge := func(before, after string) {
		b, a := strings.ToLower(before), strings.ToLower(after)
		if b == a {
			return
		}
		if _, ok := position[b]; !ok {
			return
		}
		if _, ok := position[a]; !ok {
			return
		}
		if edges[b][a] {
			return
		}
		edges[b][a] = true
		indegree[a]++
		report.Rules++
	}

	var first, last []string
	for key := range position {
		id := canonical[key]
		m := rulesFor(id)

		for _, dep := range m.Require {
			if _, present := position[strings.ToLower(dep)]; !present {
				report.Issues = append(report.Issues, OrderIssue{
					Kind: "missing", Mod: id, Other: dep,
					Detail: id + " requires " + dep + ", which is not in the load order.",
				})
				continue
			}
			addEdge(dep, id)
		}
		for _, dep := range m.LoadAfter {
			addEdge(dep, id)
		}
		for _, dep := range m.LoadBefore {
			addEdge(id, dep)
		}
		for _, other := range m.Incompatible {
			if _, present := position[strings.ToLower(other)]; present {
				report.Issues = append(report.Issues, OrderIssue{
					Kind: "incompatible", Mod: id, Other: other,
					Detail: id + " declares that it does not work alongside " + other + ".",
				})
			}
		}
		if m.LoadFirst {
			first = append(first, key)
		}
		if m.LoadLast {
			last = append(last, key)
		}
	}

	// loadFirst and loadLast are expressed as edges too, so they take part in
	// the same sort rather than being bolted on afterwards.
	for _, f := range first {
		for key := range position {
			if key != f {
				addEdge(canonical[f], canonical[key])
			}
		}
	}
	for _, l := range last {
		for key := range position {
			if key != l {
				addEdge(canonical[key], canonical[l])
			}
		}
	}

	// Kahn's algorithm, taking the ready node that came earliest in the
	// operator's own list so an unconstrained list is left exactly as it was.
	ready := make([]string, 0, len(position))
	for key, deg := range indegree {
		if deg == 0 {
			ready = append(ready, key)
		}
	}
	byPosition := func(list []string) {
		sort.SliceStable(list, func(i, j int) bool { return position[list[i]] < position[list[j]] })
	}
	byPosition(ready)

	var sorted []string
	placed := map[string]bool{}
	for len(ready) > 0 {
		key := ready[0]
		ready = ready[1:]
		sorted = append(sorted, canonical[key])
		placed[key] = true

		var freed []string
		for next := range edges[key] {
			indegree[next]--
			if indegree[next] == 0 {
				freed = append(freed, next)
			}
		}
		byPosition(freed)
		ready = append(ready, freed...)
		byPosition(ready)
	}

	// Anything left is in a cycle. Report it and keep the original order for
	// those, rather than dropping mods or inventing an order.
	if len(sorted) < len(position) {
		var stuck []string
		for key := range position {
			if !placed[key] {
				stuck = append(stuck, key)
			}
		}
		byPosition(stuck)
		names := make([]string, 0, len(stuck))
		for _, key := range stuck {
			names = append(names, canonical[key])
			sorted = append(sorted, canonical[key])
		}
		report.Issues = append(report.Issues, OrderIssue{
			Kind: "cycle", Mod: strings.Join(names, ", "),
			Detail: "These mods depend on each other in a loop, so no order satisfies all of them: " +
				strings.Join(names, ", ") + ". They were left in their current order.",
		})
	}

	report.Order = sorted
	report.Changed = !sameOrder(order, sorted)
	if report.Changed {
		for i, id := range sorted {
			old := position[strings.ToLower(id)]
			if old != i {
				report.Issues = append(report.Issues, OrderIssue{
					Kind: "moved", Mod: id,
					Detail: fmt.Sprintf("%s moved from position %d to %d.", id, old+1, i+1),
				})
			}
		}
	}
	sort.SliceStable(report.Issues, func(i, j int) bool {
		return issueRank(report.Issues[i].Kind) < issueRank(report.Issues[j].Kind)
	})
	return report
}

func issueRank(kind string) int {
	switch kind {
	case "cycle":
		return 0
	case "missing":
		return 1
	case "incompatible":
		return 2
	case "duplicate":
		return 3
	default:
		return 4
	}
}

func sameOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// LoadSortingRules reads the community sorting_rules.txt, which carries
// ordering constraints for mods whose authors did not declare any. The format
// is a mod ID in square brackets followed by loadAfter/loadBefore lines.
func LoadSortingRules(path string) SortRules {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	rules := SortRules{}
	var current string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "--") || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			if current != "" {
				if _, ok := rules[current]; !ok {
					rules[current] = Mod{ID: current}
				}
			}
			continue
		}
		if current == "" {
			continue
		}
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:eq]))
		value := strings.TrimSpace(line[eq+1:])
		m := rules[current]
		switch key {
		case "loadafter":
			m.LoadAfter = append(m.LoadAfter, splitModList(value)...)
		case "loadbefore":
			m.LoadBefore = append(m.LoadBefore, splitModList(value)...)
		case "incompatiblemods":
			m.Incompatible = append(m.Incompatible, splitModList(value)...)
		case "require":
			m.Require = append(m.Require, splitModList(value)...)
		case "loadfirst":
			m.LoadFirst = isOn(value)
		case "loadlast":
			m.LoadLast = isOn(value)
		}
		rules[current] = m
	}
	return rules
}

// SortingRulesPath returns where the community rules file would live for a
// server, or an empty string if there is nowhere to look.
func (l Layout) SortingRulesPath() string {
	for _, root := range candidateRoots(l.Base) {
		candidate := joinPath(root, "Lua", "sorting_rules.txt")
		if fileExists(candidate) {
			return candidate
		}
	}
	return ""
}
