package pz

import (
	"regexp"
	"strconv"
	"strings"
)

// The game documents its own settings. The ini it writes has a "#" comment
// above each key, and the sandbox file a "--" comment, both with the meaning,
// often the range ("Min: 0 Max: 1000 Default: 2"), and for sandbox choices a
// line per value ("1 = Insane"). Reading those gives every setting a real
// description and every choice its label, straight from the build that wrote
// the file, instead of a table here that goes stale.

// Option is one labelled value of a choice setting.
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type fieldDoc struct {
	help     string
	options  []Option
	min, max *int
}

var (
	reDocOption = regexp.MustCompile(`^\s*(-?\d+)\s*=\s*(.+?)\s*$`)
	reDocRange  = regexp.MustCompile(`(?i)\bMin:\s*(-?[\d.]+)\s*Max:\s*(-?[\d.]+)(?:\s*Default:\s*\S+)?`)
)

// parseDoc reads the comment lines above one setting, prefixes already
// removed, in file order.
func parseDoc(lines []string) fieldDoc {
	var d fieldDoc
	var help []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if m := reDocOption.FindStringSubmatch(l); m != nil {
			d.options = append(d.options, Option{Value: m[1], Label: m[2]})
			continue
		}
		if m := reDocRange.FindStringSubmatchIndex(l); m != nil {
			lo, errLo := strconv.Atoi(l[m[2]:m[3]])
			hi, errHi := strconv.Atoi(l[m[4]:m[5]])
			if errLo == nil && errHi == nil && d.min == nil {
				d.min, d.max = &lo, &hi
			}
			l = strings.TrimSpace(l[:m[0]] + l[m[1]:])
			if l == "" {
				continue
			}
		}
		help = append(help, l)
	}
	d.help = strings.Join(help, " ")
	return d
}

// commentAbove collects the comment block directly above line n, in order.
func commentAbove(lines []string, n int, prefix string) []string {
	var out []string
	for i := n - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(t, prefix) {
			break
		}
		out = append(out, strings.TrimSpace(strings.TrimPrefix(t, prefix)))
	}
	for a, b := 0, len(out)-1; a < b; a, b = a+1, b-1 {
		out[a], out[b] = out[b], out[a]
	}
	return out
}

// applyDoc fills in what the game's comment says, without overriding what
// is already known. A choice is only made a choice when the current value is
// one of its options; otherwise the options stay in the help text, so a value
// set by a mod or by hand is never silently replaced.
func applyDoc(f *Field, d fieldDoc) {
	if f.Help == "" {
		f.Help = d.help
	}
	if f.Min == nil && f.Max == nil && d.min != nil && f.Type == FieldInt {
		f.Min, f.Max = d.min, d.max
	}
	if len(d.options) == 0 || f.Type != FieldInt {
		return
	}
	for _, o := range d.options {
		if o.Value == strings.TrimSpace(f.Value) {
			f.Type = FieldChoice
			f.Options = d.options
			f.Choices = make([]string, len(d.options))
			for i, o := range d.options {
				f.Choices[i] = o.Value
			}
			return
		}
	}
	labels := make([]string, len(d.options))
	for i, o := range d.options {
		labels[i] = o.Value + " = " + o.Label
	}
	f.Help = strings.TrimSpace(f.Help + " (" + strings.Join(labels, ", ") + ")")
}
