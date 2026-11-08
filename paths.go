package main

import (
	"os"
	"path/filepath"
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
