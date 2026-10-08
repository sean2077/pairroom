package ccswitch

import (
	"strconv"
	"strings"
)

type tomlDocument struct {
	Root              map[string]string
	Sections          map[string]map[string]string
	Unsupported       map[string]bool
	Invalid           bool
	UnsafeCredentials bool
}

// parseTOML reads the scalar provider subset, including quoted table keys and
// literal strings emitted by CC Switch. Composite values are retained only as
// empty markers, so unsupported authentication fields cannot disappear. Their
// contents, and array-of-table entries, never become provider scalars. Shared
// hooks/MCP/configuration are not interpreted or copied into a child process.
func parseTOML(input string) tomlDocument {
	doc := tomlDocument{
		Root: make(map[string]string), Sections: make(map[string]map[string]string),
		Unsupported: make(map[string]bool),
	}
	current := doc.Root
	currentPath := ""
	keys, tables, arrayTables := make(map[string]bool), make(map[string]bool), make(map[string]bool)
	parents := make(map[string]bool)
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
			if tomlHasAncestor(keys, name) || keys[name] {
				doc.Invalid = true
			}
			if array {
				if tables[name] {
					doc.Invalid = true
				}
				arrayTables[name] = true
				tomlMarkParents(parents, name)
				doc.Unsupported[name] = true
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
		}
		if tomlAuthContainer(path) || current == nil && tomlOpaqueCredential(path) {
			doc.UnsafeCredentials = true
		}
		value := strings.TrimSpace(rawValue)
		scalar, supported := "", false
		if strings.HasPrefix(value, "[") || strings.HasPrefix(value, "{") || strings.HasPrefix(value, `"""`) || strings.HasPrefix(value, "'''") {
			var closed bool
			index, closed = skipTOMLValue(lines, index, value)
			if !closed {
				doc.Invalid = true
			}
		} else {
			value = strings.TrimSpace(stripComment(value))
			scalar, supported = tomlScalar(value)
			if value == "" || !supported && (strings.HasPrefix(value, `"`) || strings.HasPrefix(value, "'")) {
				doc.Invalid = true
			}
		}
		if !supported {
			doc.Unsupported[path] = true
			if tomlOpaqueCredential(path) {
				doc.UnsafeCredentials = true
			}
		}
		if current != nil {
			// A present but invalid declaration must not enable fallback to an
			// unrelated credential or an earlier duplicate value.
			current[key] = scalar
		}
	}
	// Malformed syntax prevents reliable credential inspection as well as
	// materialization. Callers must withhold its uninspectable public metadata.
	doc.UnsafeCredentials = doc.UnsafeCredentials || doc.Invalid
	return doc
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
			var ok bool
			part, ok = tomlScalar(part)
			if !ok {
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
	last := strings.ToLower(parts[len(parts)-1])
	return tomlAuthContainer(path) || credentialFieldName(last) || last == "env_key" ||
		len(parts) <= 2 && (parts[0] == "model_providers" || parts[0] == "model")
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

func tomlScalar(value string) (string, bool) {
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		unquoted, err := strconv.Unquote(value)
		return unquoted, err == nil
	}
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' && !strings.Contains(value[1:len(value)-1], "'") {
		return value[1 : len(value)-1], true
	}
	if value == "true" || value == "false" {
		return value, true
	}
	if _, err := strconv.ParseInt(value, 10, 64); err == nil {
		return value, true
	}
	return "", false
}
