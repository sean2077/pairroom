package prompt

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sean2077/pairroom/internal/model"
)

var (
	mentionPattern = regexp.MustCompile(`(?i)(?:^|[^[:alnum:]_])@([[:alnum:]_]+)\b`)
	urlPattern     = regexp.MustCompile(`(?i)\b(?:[a-z][a-z0-9+.-]*://|www\.)\S+|\b(?:[a-z0-9-]+\.)+[a-z]{2,}(?::[0-9]+)?(?:[/?#]\S*)?|\b(?:localhost|(?:[0-9]{1,3}\.){3}[0-9]{1,3})(?::[0-9]+)?(?:[/?#]\S*)?|\[[0-9a-f:]+\](?::[0-9]+)?(?:[/?#]\S*)?`)
	emailPattern   = regexp.MustCompile(`(?i)\b[[:alnum:]._%+\-]+@[[:alnum:].\-]+\.[[:alpha:]]{2,}\b`)
)

type MentionResult struct {
	Targets        []model.ActorID
	Human          bool
	Ambiguous      []string
	RemovedAliases []string
}

func Mentions(text string, sender model.ActorID) []model.ActorID {
	return MentionsWithRuntimes(text, sender, nil)
}

func MentionsWithRuntimes(text string, sender model.ActorID, runtimes map[model.ActorID]model.RuntimeKind) []model.ActorID {
	return ParseMentions(text, sender, runtimes).Targets
}

func ParseMentions(text string, sender model.ActorID, runtimes map[model.ActorID]model.RuntimeKind) MentionResult {
	identities := model.ParticipantIdentities(runtimes)
	byHandle := make(map[string]model.ActorID, len(identities))
	runtimeCounts := make(map[string]int, len(identities))
	for actor, identity := range identities {
		byHandle[strings.TrimPrefix(strings.ToLower(identity.MentionHandle), "@")] = actor
		kind := runtimes[actor].CanonicalForSlot(actor)
		runtimeCounts[string(kind)]++
	}

	clean := stripMarkdownCodeAndURLs(text)
	matches := mentionPattern.FindAllStringSubmatchIndex(clean, -1)
	var targets []model.ActorID
	result := MentionResult{}
	ambiguous := make(map[string]struct{})
	removed := make(map[string]struct{})
	for _, match := range matches {
		if len(match) < 4 {
			continue
		}
		mentionAt := match[2] - 1 // the capture starts immediately after '@'
		if mentionHasPrefixContinuation(clean, mentionAt) {
			continue
		}
		// The regexp intentionally keeps the trailing delimiter out so adjacent
		// mentions remain discoverable. Reject delimiter-like continuations here
		// to keep handles exact (`@codex-build` and `@codex.dev` are not `@codex`).
		if mentionHasContinuation(clean, match[3]) {
			continue
		}
		name := strings.ToLower(clean[match[2]:match[3]])
		if name == "user" {
			result.Human = true
			continue
		}
		switch name {
		case "peer", "human", "all", "agent1", "agent2":
			removed["@"+name] = struct{}{}
			continue
		}
		if actor, ok := byHandle[name]; ok {
			if actor != sender {
				targets = append(targets, actor)
			}
			continue
		}
		if runtimeCounts[name] > 1 {
			ambiguous["@"+name] = struct{}{}
		}
	}
	result.Targets = model.NormalizeActors(targets)
	for _, name := range []string{"@claude", "@codex", "@grok"} {
		if _, ok := ambiguous[name]; ok {
			result.Ambiguous = append(result.Ambiguous, name)
		}
	}
	for _, name := range []string{"@peer", "@human", "@all", "@agent1", "@agent2"} {
		if _, ok := removed[name]; ok {
			result.RemovedAliases = append(result.RemovedAliases, name)
		}
	}
	return result
}

func stripMarkdownCodeAndURLs(text string) string {
	// Mask complete email addresses first. Otherwise the URL/domain pass can
	// erase only the domain (`example.com`) and leave an email local part such
	// as `@codex` looking like a real mention.
	text = emailPattern.ReplaceAllStringFunc(text, func(value string) string { return strings.Repeat(" ", len(value)) })
	text = urlPattern.ReplaceAllStringFunc(text, func(value string) string { return strings.Repeat(" ", len(value)) })
	bytes := []byte(text)
	out := append([]byte(nil), bytes...)
	for i := 0; i < len(bytes); {
		if bytes[i] != '`' && bytes[i] != '~' {
			i++
			continue
		}
		marker := bytes[i]
		run := 1
		for i+run < len(bytes) && bytes[i+run] == marker {
			run++
		}
		if marker == '~' && run < 3 {
			i += run
			continue
		}
		width := run
		end := i + width
		for end < len(bytes) {
			matched := true
			for n := 0; n < width; n++ {
				if end+n >= len(bytes) || bytes[end+n] != marker {
					matched = false
					break
				}
			}
			if matched {
				end += width
				break
			}
			end++
		}
		if end > len(bytes) {
			end = len(bytes)
		}
		for n := i; n < end; n++ {
			if out[n] != '\n' && out[n] != '\r' {
				out[n] = ' '
			}
		}
		i = end
	}
	// Markdown also treats a tab or four leading spaces as an indented code
	// block. Mask those lines after fenced-code handling so a literal handle in
	// generated help/output cannot become a relay request.
	for start := 0; start < len(out); {
		end := start
		for end < len(out) && out[end] != '\n' && out[end] != '\r' {
			end++
		}
		line := bytes[start:end]
		indent := indentedCodeLine(line)
		if indent {
			for n := start; n < end; n++ {
				if out[n] != '\n' && out[n] != '\r' {
					out[n] = ' '
				}
			}
		}
		if end >= len(out) {
			break
		}
		end++
		if end < len(out) && out[end-1] == '\r' && out[end] == '\n' {
			end++
		}
		start = end
	}
	return string(out)
}

func isMentionContinuation(value byte) bool {
	return value == '.' || value == '-' || value == '_' ||
		(value >= '0' && value <= '9') ||
		(value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z')
}

func mentionHasPrefixContinuation(text string, at int) bool {
	if at <= 0 || at > len(text) {
		return false
	}
	if text[at-1] == '@' {
		// `@@codex` is not a second exact mention; treating it as one would make
		// typoed handles unexpectedly trigger a relay.
		return true
	}
	backslashes := 0
	for index := at - 1; index >= 0 && text[index] == '\\'; index-- {
		backslashes++
	}
	if backslashes%2 == 1 {
		// Markdown's escaped `\@handle` is visible prose, not an address.
		return true
	}
	r, size := utf8.DecodeLastRuneInString(text[:at])
	if size == 0 {
		return false
	}
	if size == 1 && r == utf8.RuneError {
		return true
	}
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r)
}

func indentedCodeLine(line []byte) bool {
	// Markdown permits block quotes to contain an indented code block. Strip
	// up to the repeated `> ` prefixes before checking the four-space/tab rule.
	offset := 0
	for {
		probe := offset
		for probe < len(line) && probe-offset < 3 && line[probe] == ' ' {
			probe++
		}
		if probe >= len(line) || line[probe] != '>' {
			break
		}
		offset = probe + 1
		if offset < len(line) && line[offset] == ' ' {
			offset++
		}
	}
	if offset >= len(line) {
		return false
	}
	if line[offset] == '\t' {
		return true
	}
	return offset+4 <= len(line) && line[offset] == ' ' && line[offset+1] == ' ' && line[offset+2] == ' ' && line[offset+3] == ' '
}

func mentionHasContinuation(text string, index int) bool {
	if index >= len(text) {
		return false
	}
	r, size := utf8.DecodeRuneInString(text[index:])
	if size == 0 {
		return false
	}
	if size == 1 && r == utf8.RuneError {
		// A malformed byte immediately after a handle is not a trustworthy
		// token boundary; fail closed instead of routing a corrupted string.
		return true
	}
	return isMentionContinuation(text[index]) || unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r)
}

func MentionsHuman(text string) bool {
	clean := stripMarkdownCodeAndURLs(text)
	for _, match := range mentionPattern.FindAllStringSubmatchIndex(clean, -1) {
		if len(match) >= 4 && !mentionHasPrefixContinuation(clean, match[2]-1) && !mentionHasContinuation(clean, match[3]) && strings.EqualFold(clean[match[2]:match[3]], "user") {
			return true
		}
	}
	return false
}
