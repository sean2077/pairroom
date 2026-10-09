package ccswitch

import (
	"strconv"
	"strings"
)

type tomlDocument struct {
	Root               map[string]string
	Sections           map[string]map[string]string
	ScalarKinds        map[string]tomlScalarKind
	Unsupported        map[string]bool
	UnsupportedParents map[string]bool
	Tables             map[string]bool
	Invalid            bool
	UnsafeCredentials  bool
}

type tomlScalarKind uint8

const (
	tomlString tomlScalarKind = iota + 1
	tomlBoolean
	tomlInteger
	// A string-valued option historically accepts every supported scalar and
	// emits its text in quotes. Identity, routing, and credentials use tomlString.
	tomlScalarText
)

// parseTOML reads the scalar provider subset, including quoted table keys and
// literal strings emitted by CC Switch. Composite values are retained only as
// empty markers, so unsupported authentication fields cannot disappear. Their
// contents, and array-of-table entries, never become provider scalars. Shared
// hooks/MCP/configuration are not interpreted or copied into a child process.
func parseTOML(input string) tomlDocument {
	doc := tomlDocument{
		Root: make(map[string]string), Sections: make(map[string]map[string]string),
		ScalarKinds: make(map[string]tomlScalarKind), Unsupported: make(map[string]bool),
		UnsupportedParents: make(map[string]bool), Tables: make(map[string]bool),
	}
	current := doc.Root
	currentPath := ""
	keys, tables, arrayTables := make(map[string]bool), make(map[string]bool), make(map[string]bool)
	dottedTables := make(map[string]bool)
	parents := doc.Tables
	lines := strings.Split(input, "\n")
	for index := 0; index < len(lines); index++ {
		line := strings.TrimSpace(lines[index])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			line = strings.TrimSpace(stripComment(line))
			current = nil
			array := strings.HasPrefix(line, "[[")
			width := 1
			if array {
				width = 2
			}
			if len(line) < 2*width || !strings.HasSuffix(line, strings.Repeat("]", width)) {
				doc.Invalid = true
				continue
			}
			name := normalizeTOMLPath(line[width : len(line)-width])
			if name == "" {
				doc.Invalid = true
				continue
			}
			currentPath = name
			if tomlAuthContainer(name) {
				doc.UnsafeCredentials = true
			}
			if tomlHasAncestor(keys, name) || keys[name] || dottedTables[name] {
				doc.Invalid = true
			}
			if array {
				if tables[name] {
					doc.Invalid = true
				}
				arrayTables[name] = true
				tomlMarkParents(parents, name)
				parents[name] = true
				markUnsupportedTOML(&doc, name)
				if tomlOpaqueCredential(name) {
					doc.UnsafeCredentials = true
				}
				continue
			}
			if arrayTables[name] {
				doc.Invalid = true
			}
			// Repeated array entries and their subtables have distinct identities.
			// They are shared opaque configuration, never provider scalar tables.
			if tomlHasAncestor(arrayTables, name) {
				continue
			}
			if tables[name] {
				doc.Invalid = true
			}
			tables[name] = true
			tomlMarkParents(parents, name)
			parents[name] = true
			if doc.Sections[name] == nil {
				doc.Sections[name] = make(map[string]string)
			}
			current = doc.Sections[name]
			continue
		}
		key, rawValue, ok := splitTOMLAssignment(line)
		if !ok {
			doc.Invalid = true
			continue
		}
		key = normalizeTOMLPath(key)
		if key == "" {
			doc.Invalid = true
			continue
		}
		path := key
		if currentPath != "" {
			path = currentPath + "." + key
		}
		if current != nil {
			if keys[path] || tables[path] || arrayTables[path] || parents[path] || tomlHasAncestor(keys, path) {
				doc.Invalid = true
			}
			keys[path] = true
			tomlMarkParents(parents, path)
			for _, parent := range tomlParents(key) {
				if currentPath != "" {
					parent = currentPath + "." + parent
				}
				if tables[parent] || arrayTables[parent] {
					doc.Invalid = true
				}
				dottedTables[parent] = true
			}
		}
		if tomlAuthContainer(path) || current == nil && tomlOpaqueCredential(path) {
			doc.UnsafeCredentials = true
		}
		value := strings.TrimSpace(rawValue)
		var scalar string
		var kind tomlScalarKind
		if strings.HasPrefix(value, "[") || strings.HasPrefix(value, "{") || strings.HasPrefix(value, `"""`) || strings.HasPrefix(value, "'''") {
			var closed bool
			index, closed = skipTOMLValue(lines, index, value)
			if !closed {
				doc.Invalid = true
			}
		} else {
			value = strings.TrimSpace(stripComment(value))
			scalar, kind = tomlScalar(value)
			if value == "" || kind == 0 && (strings.HasPrefix(value, `"`) || strings.HasPrefix(value, "'")) {
				doc.Invalid = true
			}
		}
		if kind == 0 {
			markUnsupportedTOML(&doc, path)
			if tomlOpaqueCredential(path) {
				doc.UnsafeCredentials = true
			}
		}
		if current != nil {
			// A present but invalid declaration must not enable fallback to an
			// unrelated credential or an earlier duplicate value.
			storeTOMLScalar(&doc, path, scalar)
			if kind != 0 {
				doc.ScalarKinds[path] = kind
			}
		}
	}
	// Keep generic syntax errors separate from explicit unsafe credential data.
	// The active config rejects either; an unused metadata snapshot may contain
	// ordinary non-TOML text, while opaque credential declarations still matter.
	return doc
}

// Dotted assignments and table headings describe the same TOML paths. Store
// each scalar by that path, rather than by the physical table where it was
// written; otherwise a supported Provider option can silently disappear.
func storeTOMLScalar(doc *tomlDocument, path, value string) {
	parts, ok := tomlPathParts(path)
	if !ok || len(parts) == 0 {
		doc.Invalid = true
		return
	}
	key := tomlKey(parts[len(parts)-1])
	parent := tomlParent(path)
	if parent == "" {
		doc.Root[key] = value
		return
	}
	if doc.Sections[parent] == nil {
		doc.Sections[parent] = make(map[string]string)
	}
	doc.Sections[parent][key] = value
}

func tomlParent(path string) string {
	parents := tomlParents(path)
	if len(parents) == 0 {
		return ""
	}
	return parents[len(parents)-1]
}

func tomlScalarValue(doc tomlDocument, path string) string {
	parent := tomlParent(path)
	if parent == "" {
		return doc.Root[path]
	}
	return doc.Sections[parent][strings.TrimPrefix(path, parent+".")]
}

// An absent option inherits native configuration. A table, opaque ancestor,
// composite value, or scalar of the wrong type is a declaration, not absence.
func supportedTOMLScalar(doc tomlDocument, path string, want tomlScalarKind) bool {
	if kind, present := doc.ScalarKinds[path]; present {
		if kind == want || want == tomlScalarText {
			return true
		}
		// Older Profiles store some boolean/integer options as strings. The
		// mapper emits their canonical scalar values; retain that compatibility
		// without coercing a number or boolean into a model or credential.
		if kind == tomlString && want != tomlString {
			_, parsed := tomlScalar(tomlScalarValue(doc, path))
			return parsed == want
		}
		return false
	}
	for _, parent := range tomlParents(path) {
		if _, present := doc.ScalarKinds[parent]; present {
			return false
		}
	}
	return !unsupportedTOMLPrefix(doc, path) && !doc.Tables[path]
}

// Index unsupported descendants while parsing. All declaration checks then use
// these same canonical paths, without rescanning the document for each option.
func markUnsupportedTOML(doc *tomlDocument, path string) {
	doc.Unsupported[path] = true
	tomlMarkParents(doc.UnsupportedParents, path)
}

func unsupportedTOMLPrefix(doc tomlDocument, path string) bool {
	return doc.Unsupported[path] || doc.UnsupportedParents[path] || tomlHasAncestor(doc.Unsupported, path)
}

func normalizeTOMLPath(path string) string {
	parts, ok := tomlPathParts(path)
	if !ok {
		return ""
	}
	for i, part := range parts {
		parts[i] = tomlKey(part)
	}
	return strings.Join(parts, ".")
}

func tomlPathParts(path string) ([]string, bool) {
	var parts []string
	start := 0
	var quote rune
	escaped := false
	for i, r := range path {
		if escaped {
			escaped = false
			continue
		}
		if quote != 0 {
			if r == '\\' && quote == '"' {
				escaped = true
			} else if r == quote {
				quote = 0
			}
			continue
		}
		if r == '"' || r == '\'' {
			quote = r
		} else if r == '.' {
			parts = append(parts, path[start:i])
			start = i + 1
		}
	}
	if quote != 0 {
		return nil, false
	}
	parts = append(parts, path[start:])
	for i, part := range parts {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, `"`) || strings.HasPrefix(part, "'") {
			var kind tomlScalarKind
			part, kind = tomlScalar(part)
			if kind != tomlString {
				return nil, false
			}
		} else if !bareTOMLKey(part) {
			return nil, false
		}
		parts[i] = part
	}
	return parts, true
}

func tomlHasAncestor(paths map[string]bool, path string) bool {
	for _, parent := range tomlParents(path) {
		if paths[parent] {
			return true
		}
	}
	return false
}

func tomlMarkParents(parents map[string]bool, path string) {
	for _, parent := range tomlParents(path) {
		parents[parent] = true
	}
}

func tomlParents(path string) []string {
	parts, _ := tomlPathParts(path)
	var parents []string
	var prefix string
	for i := 0; i+1 < len(parts); i++ {
		if i > 0 {
			prefix += "."
		}
		prefix += tomlKey(parts[i])
		parents = append(parents, prefix)
	}
	return parents
}

func tomlAuthContainer(path string) bool {
	parts, _ := tomlPathParts(path)
	for i, part := range parts {
		if i == 1 && (parts[0] == "model_providers" || parts[0] == "model") {
			continue // A provider ID is data, even when it is named "auth".
		}
		switch strings.ToLower(part) {
		case "auth", "aws", "headers", "http_headers", "env_http_headers", "query_params", "credentials":
			return true
		}
	}
	return false
}

func tomlOpaqueCredential(path string) bool {
	parts, ok := tomlPathParts(path)
	if !ok {
		return true
	}
	return tomlAuthContainer(path) || tomlCredentialField(path) != "" ||
		len(parts) <= 2 && (parts[0] == "model_providers" || parts[0] == "model")
}

// Credential ancestry matters even when a key is expressed as a table. Named
// Providers and MCP servers are data keys, not credential-container declarations.
func tomlCredentialField(path string) string {
	parts, _ := tomlPathParts(path)
	var credential string
	for i, part := range parts {
		if i == 1 && (parts[0] == "model_providers" || parts[0] == "model" || parts[0] == "mcp_servers") {
			continue
		}
		field := strings.ToLower(part)
		if credentialFieldName(field) || field == "env_key" || field == "env-key" {
			credential = field
		}
	}
	return credential
}

func splitTOMLAssignment(line string) (string, string, bool) {
	var quote rune
	escaped := false
	for i, r := range line {
		if escaped {
			escaped = false
			continue
		}
		if quote != 0 {
			if r == '\\' && quote == '"' {
				escaped = true
			} else if r == quote {
				quote = 0
			}
		} else if r == '"' || r == '\'' {
			quote = r
		} else if r == '=' {
			return line[:i], line[i+1:], true
		}
	}
	return "", "", false
}

// skipTOMLValue never interprets composite/multiline contents. Quote, escape,
// and bracket state survive physical lines, including multiline strings inside
// arrays. It stops only at the actual outer delimiter, with no trailing tokens.
func skipTOMLValue(lines []string, start int, first string) (int, bool) {
	var quote byte
	var stack []byte
	triple, escaped := false, false
	for row := start; row < len(lines); row++ {
		line := lines[row]
		if row == start {
			line = first
		}
		for i := 0; i < len(line); i++ {
			c := line[i]
			if escaped {
				escaped = false
				continue
			}
			if quote != 0 {
				if c == '\\' && quote == '"' {
					escaped = true
					continue
				}
				if c != quote {
					continue
				}
				if triple {
					run := 1
					for i+run < len(line) && line[i+run] == quote {
						run++
					}
					i += run - 1
					if run < 3 {
						continue
					}
					if run > 5 {
						return row, false
					}
				}
				quote, triple = 0, false
				if len(stack) == 0 {
					return row, strings.TrimSpace(stripComment(line[i+1:])) == ""
				}
				continue
			}
			switch c {
			case '#':
				i = len(line)
			case '"', '\'':
				quote = c
				if i+2 < len(line) && line[i+1] == c && line[i+2] == c {
					triple = true
					i += 2
				}
			case '[', '{':
				stack = append(stack, c)
			case ']', '}':
				if len(stack) == 0 || c == ']' && stack[len(stack)-1] != '[' || c == '}' && stack[len(stack)-1] != '{' {
					return row, false
				}
				stack = stack[:len(stack)-1]
				if len(stack) == 0 {
					return row, strings.TrimSpace(stripComment(line[i+1:])) == ""
				}
			}
		}
		if quote != 0 && !triple {
			return row, false // Single-line strings cannot continue on a new line.
		}
		escaped = false
	}
	return len(lines) - 1, false
}

func bareTOMLKey(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func tomlKey(value string) string {
	if bareTOMLKey(value) {
		return value
	}
	return strconv.Quote(value)
}

func stripComment(line string) string {
	var quote rune
	escaped := false
	for i, r := range line {
		if escaped {
			escaped = false
			continue
		}
		if quote != 0 {
			if r == '\\' && quote == '"' {
				escaped = true
			} else if r == quote {
				quote = 0
			}
		} else if r == '"' || r == '\'' {
			quote = r
		} else if r == '#' {
			return line[:i]
		}
	}
	return line
}

func tomlScalar(value string) (string, tomlScalarKind) {
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		unquoted, err := strconv.Unquote(value)
		if err == nil {
			return unquoted, tomlString
		}
		return "", 0
	}
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' && !strings.Contains(value[1:len(value)-1], "'") {
		return value[1 : len(value)-1], tomlString
	}
	if value == "true" || value == "false" {
		return value, tomlBoolean
	}
	if _, err := strconv.ParseInt(value, 10, 64); err == nil {
		return value, tomlInteger
	}
	return "", 0
}
