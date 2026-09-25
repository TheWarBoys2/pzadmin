// Package compose reads Docker Compose files and the .env files beside them.
//
// PZAdmin uses this for two things: working out which containers are Project
// Zomboid servers without being told, and writing compose projects for servers
// it creates itself.
//
// It does NOT rewrite the operator's own compose file. Everything PZAdmin
// generates goes in files it owns, one per server. That is a deliberate
// constraint rather than a limitation: round-tripping a hand-maintained YAML
// document through a parser and back destroys comments, ordering and
// formatting, and the first time it mangles a file somebody has been editing
// for two years the trust is gone for good. Reading can be best-effort;
// writing over someone's work cannot.
package compose

import (
	"fmt"
	"strconv"
	"strings"
)

// Kind distinguishes the three shapes a node can take.
type Kind int

const (
	// KindScalar is a single value: a string, number or boolean.
	KindScalar Kind = iota
	// KindMapping is an ordered set of key/value pairs.
	KindMapping
	// KindSequence is an ordered list.
	KindSequence
)

// Node is one value in a parsed document.
//
// Key order is preserved because compose files are read by people, and a
// service rendered with its keys shuffled is needlessly hard to diff against
// the one it was cloned from.
type Node struct {
	Kind   Kind
	Value  string
	Keys   []string
	Fields map[string]*Node
	Items  []*Node
	Line   int
}

// Get returns a child of a mapping, or nil.
func (n *Node) Get(key string) *Node {
	if n == nil || n.Kind != KindMapping {
		return nil
	}
	return n.Fields[key]
}

// Str returns a scalar's value, or "" for anything else.
func (n *Node) Str() string {
	if n == nil || n.Kind != KindScalar {
		return ""
	}
	return n.Value
}

// Strs returns a sequence of scalars. A lone scalar counts as a list of one,
// because compose accepts both spellings for env_file and several other keys.
func (n *Node) Strs() []string {
	if n == nil {
		return nil
	}
	switch n.Kind {
	case KindScalar:
		if n.Value == "" {
			return nil
		}
		return []string{n.Value}
	case KindSequence:
		out := make([]string, 0, len(n.Items))
		for _, it := range n.Items {
			if it.Kind == KindScalar {
				out = append(out, it.Value)
			}
		}
		return out
	}
	return nil
}

// IsZero reports an absent or empty node.
func (n *Node) IsZero() bool {
	if n == nil {
		return true
	}
	switch n.Kind {
	case KindScalar:
		return n.Value == ""
	case KindMapping:
		return len(n.Keys) == 0
	case KindSequence:
		return len(n.Items) == 0
	}
	return true
}

type line struct {
	indent int
	text   string
	number int
}

// Parse reads a YAML document limited to the subset compose files use:
// nested mappings, sequences, quoted and bare scalars, flow sequences,
// block scalars and comments.
//
// It has no anchors, aliases, merge keys, multiple documents or tags. Those
// are legal YAML and essentially unheard of in a compose file; when one turns
// up, Parse fails loudly rather than guessing. Discovery degrades to "tell
// PZAdmin where the server is" and nothing is silently misread.
func Parse(src []byte) (*Node, error) {
	lines, err := scanLines(string(src))
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return &Node{Kind: KindMapping, Fields: map[string]*Node{}}, nil
	}
	node, err := parseBlock(lines)
	if err != nil {
		return nil, err
	}
	return node, nil
}

// scanLines strips comments and blank lines and records indentation.
func scanLines(src string) ([]line, error) {
	var out []line
	for i, raw := range strings.Split(src, "\n") {
		number := i + 1
		raw = strings.TrimRight(raw, "\r")
		if strings.TrimSpace(raw) == "" {
			continue
		}
		if idx := strings.IndexByte(raw, '\t'); idx >= 0 && strings.TrimSpace(raw[:idx]) == "" {
			return nil, fmt.Errorf("line %d is indented with a tab; YAML does not allow that", number)
		}
		text := stripComment(raw)
		if strings.TrimSpace(text) == "" {
			continue
		}
		indent := len(text) - len(strings.TrimLeft(text, " "))
		body := strings.TrimRight(strings.TrimLeft(text, " "), " ")
		if body == "---" || body == "..." {
			return nil, fmt.Errorf("line %d starts a second YAML document, which PZAdmin does not read", number)
		}
		out = append(out, line{indent: indent, text: body, number: number})
	}
	return out, nil
}

// stripComment removes a trailing comment, respecting quotes.
//
// A '#' only begins a comment when it is at the start of the line or follows
// whitespace. Without that rule a perfectly ordinary value like a colour or a
// URL fragment loses its tail.
func stripComment(s string) string {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '#':
			if i == 0 || s[i-1] == ' ' || s[i-1] == '\t' {
				return s[:i]
			}
		}
	}
	return s
}

// parseBlock parses a run of lines that all belong to one block. The first
// line's indentation defines the block; anything deeper belongs to a child.
func parseBlock(ls []line) (*Node, error) {
	base := ls[0].indent

	type entry struct {
		head     line
		children []line
	}
	var entries []entry
	for i := 0; i < len(ls); {
		if ls[i].indent < base {
			return nil, fmt.Errorf("line %d is indented less than the block it is in", ls[i].number)
		}
		if ls[i].indent > base {
			return nil, fmt.Errorf("line %d is indented more than expected", ls[i].number)
		}
		head := ls[i]
		i++
		start := i
		for i < len(ls) && ls[i].indent > base {
			i++
		}
		entries = append(entries, entry{head: head, children: ls[start:i]})
	}

	isSeq := isSequenceEntry(entries[0].head.text)
	for _, e := range entries {
		if isSequenceEntry(e.head.text) != isSeq {
			return nil, fmt.Errorf(
				"line %d mixes list items and key/value pairs at the same level", e.head.number)
		}
	}

	if isSeq {
		node := &Node{Kind: KindSequence, Line: entries[0].head.number}
		for _, e := range entries {
			item, err := parseSequenceItem(e.head, e.children)
			if err != nil {
				return nil, err
			}
			node.Items = append(node.Items, item)
		}
		return node, nil
	}

	node := &Node{Kind: KindMapping, Fields: map[string]*Node{}, Line: entries[0].head.number}
	for _, e := range entries {
		key, rest, err := splitKey(e.head.text, e.head.number)
		if err != nil {
			return nil, err
		}
		value, err := parseValue(rest, e.children, e.head.number)
		if err != nil {
			return nil, err
		}
		if _, exists := node.Fields[key]; !exists {
			node.Keys = append(node.Keys, key)
		}
		node.Fields[key] = value
	}
	return node, nil
}

func isSequenceEntry(text string) bool {
	return text == "-" || strings.HasPrefix(text, "- ")
}

func parseSequenceItem(head line, children []line) (*Node, error) {
	// Column of the content after the dash, so that "-   key: v" lines its
	// continuation lines up correctly.
	k := 1
	for k < len(head.text) && head.text[k] == ' ' {
		k++
	}
	rest := strings.TrimSpace(head.text[min(k, len(head.text)):])

	if rest == "" {
		if len(children) == 0 {
			return &Node{Kind: KindScalar, Line: head.number}, nil
		}
		return parseBlock(children)
	}

	// A scalar item with no children is the common case.
	if !looksLikeKey(rest) {
		if len(children) > 0 {
			return nil, fmt.Errorf("line %d: a list item cannot be both a value and a block", head.number)
		}
		v, err := scalarNode(rest, head.number)
		if err != nil {
			return nil, err
		}
		return v, nil
	}

	synthetic := append([]line{{indent: head.indent + k, text: rest, number: head.number}}, children...)
	return parseBlock(synthetic)
}

// looksLikeKey reports whether text opens a mapping entry rather than being a
// plain scalar. A quoted string is never a key here, and "a: b" inside quotes
// must not be mistaken for one.
func looksLikeKey(text string) bool {
	if text == "" {
		return false
	}
	if text[0] == '"' || text[0] == '\'' || text[0] == '[' || text[0] == '{' {
		return false
	}
	idx := unquotedColon(text)
	if idx < 0 {
		return false
	}
	// "key:" at end of line, or "key: value". A bare colon inside a URL such
	// as http://x has no space after it and no space before it.
	return idx == len(text)-1 || text[idx+1] == ' '
}

// unquotedColon returns the index of the first colon outside quotes.
func unquotedColon(s string) int {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == ':':
			return i
		}
	}
	return -1
}

func splitKey(text string, number int) (string, string, error) {
	idx := unquotedColon(text)
	if idx < 0 {
		return "", "", fmt.Errorf("line %d: expected a key and a colon", number)
	}
	key := strings.TrimSpace(text[:idx])
	rest := strings.TrimSpace(text[idx+1:])
	if key == "" {
		return "", "", fmt.Errorf("line %d: empty key", number)
	}
	if unq, ok, err := unquote(key); err != nil {
		return "", "", fmt.Errorf("line %d: %w", number, err)
	} else if ok {
		key = unq
	}
	if strings.HasPrefix(key, "<<") {
		return "", "", fmt.Errorf("line %d: merge keys are not supported", number)
	}
	if strings.HasPrefix(key, "&") || strings.HasPrefix(key, "*") {
		return "", "", fmt.Errorf("line %d: anchors and aliases are not supported", number)
	}
	return key, rest, nil
}

func parseValue(rest string, children []line, number int) (*Node, error) {
	// Block scalars. The content is kept verbatim; nothing PZAdmin reads cares
	// what is inside one, but failing to consume it would break the parse.
	if rest == "|" || rest == ">" || strings.HasPrefix(rest, "|") || strings.HasPrefix(rest, ">") {
		if len(rest) == 1 || strings.ContainsAny(rest[1:], "-+0123456789") {
			var parts []string
			for _, c := range children {
				parts = append(parts, c.text)
			}
			joiner := "\n"
			if strings.HasPrefix(rest, ">") {
				joiner = " "
			}
			return &Node{Kind: KindScalar, Value: strings.Join(parts, joiner), Line: number}, nil
		}
	}

	if rest == "" {
		if len(children) == 0 {
			// "key:" with nothing under it is an empty value, which compose
			// uses for an environment variable that takes the host's value.
			return &Node{Kind: KindScalar, Line: number}, nil
		}
		return parseBlock(children)
	}

	if len(children) > 0 {
		return nil, fmt.Errorf("line %d: a key cannot have both a value and a block beneath it", number)
	}
	return scalarNode(rest, number)
}

// scalarNode builds a scalar, expanding the flow forms compose files use for
// short lists and maps.
func scalarNode(text string, number int) (*Node, error) {
	if strings.HasPrefix(text, "[") && strings.HasSuffix(text, "]") {
		node := &Node{Kind: KindSequence, Line: number}
		for _, part := range splitFlow(text[1 : len(text)-1]) {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			item, err := scalarNode(part, number)
			if err != nil {
				return nil, err
			}
			node.Items = append(node.Items, item)
		}
		return node, nil
	}
	if strings.HasPrefix(text, "{") && strings.HasSuffix(text, "}") {
		node := &Node{Kind: KindMapping, Fields: map[string]*Node{}, Line: number}
		for _, part := range splitFlow(text[1 : len(text)-1]) {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			key, rest, err := splitKey(part, number)
			if err != nil {
				return nil, err
			}
			value, err := scalarNode(rest, number)
			if err != nil {
				return nil, err
			}
			if _, exists := node.Fields[key]; !exists {
				node.Keys = append(node.Keys, key)
			}
			node.Fields[key] = value
		}
		return node, nil
	}
	// An unquoted value opening with & or * is an anchor or an alias. Reading
	// it as the literal text would quietly substitute the wrong value.
	if text[0] == '&' || text[0] == '*' {
		return nil, fmt.Errorf("line %d: anchors and aliases are not supported", number)
	}
	if unq, ok, err := unquote(text); err != nil {
		return nil, fmt.Errorf("line %d: %w", number, err)
	} else if ok {
		return &Node{Kind: KindScalar, Value: unq, Line: number}, nil
	}
	return &Node{Kind: KindScalar, Value: text, Line: number}, nil
}

// splitFlow splits a flow collection on commas that are outside quotes and
// nested brackets.
func splitFlow(s string) []string {
	var out []string
	var quote byte
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '[' || c == '{':
			depth++
		case c == ']' || c == '}':
			depth--
		case c == ',' && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

// unquote removes surrounding quotes, reporting whether the value was quoted.
func unquote(s string) (string, bool, error) {
	if len(s) < 2 {
		return s, false, nil
	}
	switch s[0] {
	case '\'':
		if s[len(s)-1] != '\'' {
			return "", false, fmt.Errorf("unterminated single-quoted value")
		}
		// In YAML a doubled single quote is an escaped one.
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'"), true, nil
	case '"':
		if s[len(s)-1] != '"' {
			return "", false, fmt.Errorf("unterminated double-quoted value")
		}
		body := s[1 : len(s)-1]
		if unq, err := strconv.Unquote(`"` + body + `"`); err == nil {
			return unq, true, nil
		}
		// Fall back to the literal text: a value that Go's unquoter rejects
		// is far more likely to be a Windows path or an odd escape than
		// something worth failing the whole file over.
		return body, true, nil
	}
	return s, false, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
