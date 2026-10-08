package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// tomlParser reads the subset of TOML that agent configuration files use: tables, dotted and quoted keys, strings, numbers, booleans, arrays and inline tables.
type tomlParser struct {
	src  string
	pos  int
	line int
}

type tomlTable = map[string]any

func parseTOML(src string) (tomlTable, error) {
	p := &tomlParser{src: src, line: 1}
	root := tomlTable{}
	current := root

	for {
		p.skipSpaceAndComments()

		if p.eof() {
			return root, nil
		}

		if p.peek() == '[' {
			table, err := p.header(root)

			if err != nil {
				return nil, err
			}

			current = table

			continue
		}

		key, err := p.key()

		if err != nil {
			return nil, err
		}

		p.skipInlineSpace()

		if !p.accept('=') {
			return nil, p.errorf("expected = after key %q", strings.Join(key, "."))
		}

		p.skipInlineSpace()
		value, err := p.value()

		if err != nil {
			return nil, err
		}

		if err := setPath(current, key, value); err != nil {
			return nil, p.errorf("%v", err)
		}

		if err := p.endOfLine(); err != nil {
			return nil, err
		}
	}
}

func (p *tomlParser) errorf(format string, args ...any) error {
	return fmt.Errorf("toml line %d: %s", p.line, fmt.Sprintf(format, args...))
}

func (p *tomlParser) eof() bool { return p.pos >= len(p.src) }

func (p *tomlParser) peek() byte {
	if p.eof() {
		return 0
	}

	return p.src[p.pos]
}

func (p *tomlParser) next() byte {
	c := p.src[p.pos]
	p.pos++

	if c == '\n' {
		p.line++
	}

	return c
}

func (p *tomlParser) accept(c byte) bool {
	if p.peek() == c {
		p.next()

		return true
	}

	return false
}

func (p *tomlParser) skipInlineSpace() {
	for !p.eof() && (p.peek() == ' ' || p.peek() == '\t') {
		p.next()
	}
}

func (p *tomlParser) skipComment() {
	if p.peek() == '#' {
		for !p.eof() && p.peek() != '\n' {
			p.next()
		}
	}
}

func (p *tomlParser) skipSpaceAndComments() {
	for !p.eof() {
		switch p.peek() {
		case ' ', '\t', '\r', '\n':
			p.next()
		case '#':
			p.skipComment()
		default:
			return
		}
	}
}

func (p *tomlParser) endOfLine() error {
	p.skipInlineSpace()
	p.skipComment()

	if p.eof() || p.accept('\n') {
		return nil
	}

	if p.accept('\r') && p.accept('\n') {
		return nil
	}

	return p.errorf("unexpected %q", string(p.peek()))
}

func (p *tomlParser) header(root tomlTable) (tomlTable, error) {
	p.next()
	array := p.accept('[')
	p.skipInlineSpace()
	key, err := p.key()

	if err != nil {
		return nil, err
	}

	p.skipInlineSpace()

	if !p.accept(']') || (array && !p.accept(']')) {
		return nil, p.errorf("unterminated table header")
	}

	if err := p.endOfLine(); err != nil {
		return nil, err
	}

	parent, err := descend(root, key[:len(key)-1])

	if err != nil {
		return nil, p.errorf("%v", err)
	}

	name := key[len(key)-1]

	if array {
		table := tomlTable{}
		list, _ := parent[name].([]any)
		parent[name] = append(list, table)

		return table, nil
	}

	switch existing := parent[name].(type) {
	case nil:
		table := tomlTable{}
		parent[name] = table

		return table, nil
	case tomlTable:
		return existing, nil
	case []any:
		if len(existing) > 0 {
			if table, ok := existing[len(existing)-1].(tomlTable); ok {
				return table, nil
			}
		}
	}

	return nil, p.errorf("%q is not a table", strings.Join(key, "."))
}

func descend(table tomlTable, path []string) (tomlTable, error) {
	for _, part := range path {
		switch child := table[part].(type) {
		case nil:
			next := tomlTable{}
			table[part] = next
			table = next
		case tomlTable:
			table = child
		case []any:
			if len(child) == 0 {
				return nil, fmt.Errorf("%q is an empty array", part)
			}

			last, ok := child[len(child)-1].(tomlTable)

			if !ok {
				return nil, fmt.Errorf("%q is not a table", part)
			}

			table = last
		default:
			return nil, fmt.Errorf("%q is not a table", part)
		}
	}

	return table, nil
}

func setPath(table tomlTable, key []string, value any) error {
	parent, err := descend(table, key[:len(key)-1])

	if err != nil {
		return err
	}

	name := key[len(key)-1]

	if _, exists := parent[name]; exists {
		return fmt.Errorf("duplicate key %q", strings.Join(key, "."))
	}

	parent[name] = value

	return nil
}

func isBareKeyChar(c byte) bool {
	return c == '_' || c == '-' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func (p *tomlParser) key() ([]string, error) {
	var parts []string

	for {
		p.skipInlineSpace()
		var part string

		switch {
		case p.peek() == '"' || p.peek() == '\'':
			s, err := p.str()

			if err != nil {
				return nil, err
			}

			part = s
		default:
			start := p.pos

			for !p.eof() && isBareKeyChar(p.peek()) {
				p.next()
			}

			if start == p.pos {
				return nil, p.errorf("expected a key")
			}

			part = p.src[start:p.pos]
		}

		parts = append(parts, part)
		p.skipInlineSpace()

		if !p.accept('.') {
			return parts, nil
		}
	}
}

func (p *tomlParser) value() (any, error) {
	switch c := p.peek(); {
	case c == '"' || c == '\'':
		return p.str()

	case c == '[':
		return p.array()

	case c == '{':
		return p.inlineTable()

	case strings.HasPrefix(p.src[p.pos:], "true"):
		p.pos += 4

		return true, nil

	case strings.HasPrefix(p.src[p.pos:], "false"):
		p.pos += 5

		return false, nil

	default:
		return p.number()
	}
}

var tomlNumberPattern = regexp.MustCompile(`^[+-]?(0x[0-9A-Fa-f_]+|0o[0-7_]+|0b[01_]+|inf|nan|[0-9_]+(\.[0-9_]+)?([eE][+-]?[0-9_]+)?)`)

// number also accepts dates and times as opaque strings, since nothing here needs them.
func (p *tomlParser) number() (any, error) {
	start := p.pos

	for !p.eof() {
		c := p.peek()

		if c == ',' || c == ']' || c == '}' || c == '\n' || c == '\r' || c == '#' {
			break
		}

		p.next()
	}

	text := strings.TrimSpace(p.src[start:p.pos])

	if text == "" {
		return nil, p.errorf("expected a value")
	}

	if !tomlNumberPattern.MatchString(text) {
		if unicode.IsDigit(rune(text[0])) {
			return text, nil
		}

		return nil, p.errorf("unexpected value %q", text)
	}

	clean := strings.ReplaceAll(text, "_", "")

	if i, err := strconv.ParseInt(clean, 0, 64); err == nil {
		return i, nil
	}

	if f, err := strconv.ParseFloat(clean, 64); err == nil {
		return f, nil
	}

	return text, nil
}

func (p *tomlParser) str() (string, error) {
	quote := p.next()
	multi := strings.HasPrefix(p.src[p.pos:], string([]byte{quote, quote}))

	if multi {
		p.pos += 2

		if p.accept('\r') {
			p.accept('\n')
		} else {
			p.accept('\n')
		}
	}

	var b strings.Builder

	for {
		if p.eof() {
			return "", p.errorf("unterminated string")
		}

		c := p.next()

		switch {
		case c == quote && multi && strings.HasPrefix(p.src[p.pos:], string([]byte{quote, quote})):
			p.pos += 2

			return b.String(), nil

		case c == quote && !multi:
			return b.String(), nil

		case c == '\n' && !multi:
			return "", p.errorf("newline in string")

		case c == '\\' && quote == '"':
			r, err := p.escape(multi)

			if err != nil {
				return "", err
			}

			b.WriteString(r)

		default:
			b.WriteByte(c)
		}
	}
}

func (p *tomlParser) escape(multi bool) (string, error) {
	if p.eof() {
		return "", p.errorf("unterminated escape")
	}

	c := p.next()

	switch c {
	case 'b':
		return "\b", nil
	case 't':
		return "\t", nil
	case 'n':
		return "\n", nil
	case 'f':
		return "\f", nil
	case 'r':
		return "\r", nil
	case '"':
		return "\"", nil
	case '\\':
		return "\\", nil
	case 'u', 'U':
		size := 4

		if c == 'U' {
			size = 8
		}

		if p.pos+size > len(p.src) {
			return "", p.errorf("short unicode escape")
		}

		code, err := strconv.ParseUint(p.src[p.pos:p.pos+size], 16, 32)

		if err != nil {
			return "", p.errorf("bad unicode escape")
		}

		p.pos += size

		return string(rune(code)), nil
	case ' ', '\t', '\r', '\n':
		if !multi {
			return "", p.errorf("bad escape")
		}

		p.skipSpaceAndComments()

		return "", nil
	}

	return "", p.errorf("bad escape \\%c", c)
}

func (p *tomlParser) array() ([]any, error) {
	p.next()
	list := []any{}

	for {
		p.skipSpaceAndComments()

		if p.accept(']') {
			return list, nil
		}

		v, err := p.value()

		if err != nil {
			return nil, err
		}

		list = append(list, v)
		p.skipSpaceAndComments()

		if p.accept(',') {
			continue
		}

		if p.accept(']') {
			return list, nil
		}

		return nil, p.errorf("expected , or ] in array")
	}
}

func (p *tomlParser) inlineTable() (tomlTable, error) {
	p.next()
	table := tomlTable{}
	p.skipInlineSpace()

	if p.accept('}') {
		return table, nil
	}

	for {
		p.skipInlineSpace()
		key, err := p.key()

		if err != nil {
			return nil, err
		}

		p.skipInlineSpace()

		if !p.accept('=') {
			return nil, p.errorf("expected = in inline table")
		}

		p.skipInlineSpace()
		v, err := p.value()

		if err != nil {
			return nil, err
		}

		if err := setPath(table, key, v); err != nil {
			return nil, p.errorf("%v", err)
		}

		p.skipInlineSpace()

		if p.accept(',') {
			continue
		}

		if p.accept('}') {
			return table, nil
		}

		return nil, p.errorf("expected , or } in inline table")
	}
}

var bareKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func tomlKey(key string) string {
	if bareKeyPattern.MatchString(key) {
		return key
	}

	return tomlString(key)
}

// tomlString writes a basic string; JSON escaping is a subset of TOML's.
func tomlString(s string) string {
	data, _ := json.Marshal(s)

	return string(data)
}

var errTOMLValue = errors.New("value cannot be written as TOML")

func tomlValue(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return tomlString(x), nil
	case bool:
		return strconv.FormatBool(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case int:
		return strconv.Itoa(x), nil
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10), nil
		}

		return strconv.FormatFloat(x, 'g', -1, 64), nil
	case json.Number:
		return x.String(), nil
	case []any:
		parts := make([]string, 0, len(x))

		for _, item := range x {
			s, err := tomlValue(item)

			if err != nil {
				return "", err
			}

			parts = append(parts, s)
		}

		return "[" + strings.Join(parts, ", ") + "]", nil
	case []string:
		list := make([]any, len(x))

		for i, s := range x {
			list[i] = s
		}

		return tomlValue(list)
	}

	return "", fmt.Errorf("%w: %T", errTOMLValue, v)
}

func writeTOMLTable(b *strings.Builder, path []string, table tomlTable) error {
	keys := make([]string, 0, len(table))

	for k := range table {
		keys = append(keys, k)
	}

	sort.Strings(keys)
	quoted := make([]string, len(path))

	for i, part := range path {
		quoted[i] = tomlKey(part)
	}

	fmt.Fprintf(b, "[%s]\n", strings.Join(quoted, "."))
	var nested []string

	for _, k := range keys {
		switch v := table[k].(type) {
		case map[string]any:
			nested = append(nested, k)
		default:
			s, err := tomlValue(v)

			if err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}

			fmt.Fprintf(b, "%s = %s\n", tomlKey(k), s)
		}
	}

	for _, k := range nested {
		b.WriteString("\n")

		if err := writeTOMLTable(b, append(append([]string{}, path...), k), table[k].(map[string]any)); err != nil {
			return err
		}
	}

	return nil
}
