package agent

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// isBatchLauncher reports whether a resolved executable is a Windows batch
// file. npm installs vendor CLIs as `.cmd` shims, and Windows runs those
// through cmd.exe, which re-parses the command line with its own rules. Go's
// argument quoting targets CommandLineToArgvW, so a value such as
// `x" & calc & "` would escape its quotes and run as a separate command.
func isBatchLauncher(goos, path string) bool {
	if goos != "windows" {
		return false
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".cmd", ".bat":
		return true
	}
	return false
}

// cmdMetacharacter reports characters that cmd.exe interprets while it
// re-parses a batch launcher's command line: command separators and
// redirection, its escape character, variable expansion (`%`, and `!` under
// delayed expansion), and line breaks. A double quote only toggles cmd.exe's
// quoting state; it matters only in combination with these characters, which
// are rejected everywhere on the line, so quotes stay allowed (CC Switch
// provider arguments carry TOML-quoted values).
func cmdMetacharacter(r rune) bool {
	switch r {
	case '&', '|', '<', '>', '^', '%', '!':
		return true
	}
	return unicode.IsControl(r)
}

// checkBatchLauncherArgs fails closed when any argument would be re-parsed by
// cmd.exe as a command, redirection, or variable reference. Values are
// reported truncated; argv never carries secrets by contract.
func checkBatchLauncherArgs(runtime, path string, args []string) error {
	for _, arg := range args {
		index := strings.IndexFunc(arg, cmdMetacharacter)
		if index < 0 {
			continue
		}
		bad, _ := utf8.DecodeRuneInString(arg[index:])
		shown := []rune(arg)
		if len(shown) > 80 {
			shown = append(shown[:79], '…')
		}
		return fmt.Errorf("%s launcher %q is a Windows batch file, and argument %q contains %q, which cmd.exe would interpret; remove the character from the setting or configure the runtime command to the CLI executable instead of its .cmd shim", runtime, filepath.Base(path), string(shown), string(bad))
	}
	return nil
}

// cmdSafeDisplayText replaces cmd.exe metacharacters in display metadata
// (the native session name derived from the Room name) with visually similar
// full-width forms, so a Room name such as `R&D` still starts the runtime.
func cmdSafeDisplayText(value string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '&', '|', '<', '>', '^', '%', '!', '"':
			return r + 0xFEE0 // full-width form of the ASCII character
		}
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
}
