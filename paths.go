package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// configDir holds proxies.txt: $XDG_CONFIG_HOME/codep or ~/.config/codep.
func configDir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "codep")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "codep")
}

func defaultProxiesPath() string {
	return filepath.Join(configDir(), "proxies.txt")
}

// dataDir is where the separate VS Code instance keeps its state
// (~/Library/Application Support/codep/<app> on macOS).
func dataDir(app string) string {
	base, err := os.UserConfigDir()
	if err != nil {
		base = configDir()
	}
	return filepath.Join(base, "codep", slug(app))
}

func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// appSupportName maps an app name to its folder in ~/Library/Application Support,
// where its user settings live. Empty for unknown apps.
func appSupportName(app string) string {
	switch app {
	case "Visual Studio Code":
		return "Code"
	case "Visual Studio Code - Insiders":
		return "Code - Insiders"
	case "Cursor", "Windsurf", "VSCodium":
		return app
	}
	return ""
}

// shellRCFiles are the shell startup files checked for a conflicting codep
// alias or function, relative to the home folder.
var shellRCFiles = []string{".zshrc", ".zprofile", ".zshenv", ".bashrc", ".bash_profile", ".profile", ".config/fish/config.fish"}

var conflictRE = regexp.MustCompile(`^\s*(alias\s+codep[=\s]|codep\s*\(\)|function\s+codep\b)`)

// shellConflicts finds aliases and functions named codep in shell startup
// files. The shell runs them instead of this program, so `codep uninstall`
// would never reach it. Returns "file:line" entries.
func shellConflicts(home string) []string {
	var found []string
	for _, name := range shellRCFiles {
		path := filepath.Join(home, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(data), "\n") {
			if conflictRE.MatchString(line) {
				found = append(found, fmt.Sprintf("%s:%d", path, i+1))
			}
		}
	}
	return found
}

func warnShellConflicts() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	c := shellConflicts(home)
	if len(c) == 0 {
		return
	}
	fmt.Fprintln(os.Stderr, "\nWarning: your shell defines its own codep, which runs instead of this program:")
	for _, f := range c {
		fmt.Fprintln(os.Stderr, "  "+f)
	}
	fmt.Fprintln(os.Stderr, "Remove it and open a new terminal. Until then call this program as `command codep ...`.")
}
