package main

import (
	"errors"
	"fmt"
	"html"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const launchdLabel = "io.github.codep"

func launchdPlist(label string, program []string, logPath string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + html.EscapeString(label) + `</string>
  <key>ProgramArguments</key>
  <array>
`)
	for _, a := range program {
		b.WriteString("    <string>" + html.EscapeString(a) + "</string>\n")
	}
	b.WriteString(`  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ProcessType</key>
  <string>Background</string>
  <key>StandardOutPath</key>
  <string>` + html.EscapeString(logPath) + `</string>
  <key>StandardErrorPath</key>
  <string>` + html.EscapeString(logPath) + `</string>
</dict>
</plist>
`)
	return b.String()
}

func launchdPaths() (plist, logFile string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"),
		filepath.Join(home, "Library", "Logs", "codep.log"), nil
}

func launchctl(args ...string) (string, error) {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func runInstall(args []string) error {
	if runtime.GOOS != "darwin" {
		return errors.New("install is supported on macOS only for now; run `codep serve` under your service manager (systemd, etc.)")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if strings.Contains(exe, "go-build") {
		return errors.New("run install from a built binary, not `go run`")
	}
	plistPath, logPath, err := launchdPaths()
	if err != nil {
		return err
	}

	program := []string{exe, "serve"}
	hasProxies := flagValue(args, "proxies", "\x00") != "\x00"
	proxies := defaultProxiesPath()
	if !hasProxies {
		program = append(program, "-proxies", proxies)
		if err := ensureProxiesFile(proxies); err != nil {
			return err
		}
	}
	program = append(program, args...)

	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(plistPath, []byte(launchdPlist(launchdLabel, program, logPath)), 0o644); err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	launchctl("bootout", domain+"/"+launchdLabel) // not loaded yet is fine
	if out, err := launchctl("bootstrap", domain, plistPath); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, out)
	}

	listen := flagValue(args, "listen", defaultSocks)
	up := false
	for i := 0; i < 30 && !up; i++ {
		if c, err := net.DialTimeout("tcp", listen, 200*time.Millisecond); err == nil {
			c.Close()
			up = true
		} else {
			time.Sleep(100 * time.Millisecond)
		}
	}
	fmt.Printf("Installed: the bridge runs in the background and starts at login.\n")
	fmt.Printf("  service:   %s\n  log:       %s\n", plistPath, logPath)
	if !hasProxies {
		fmt.Printf("  proxies:   %s\n", proxies)
	}
	if dash := flagValue(args, "dashboard", defaultDashboard); dash != "" {
		fmt.Printf("  dashboard: http://%s\n", dash)
	}
	if !up {
		fmt.Printf("The bridge has not started listening on %s yet, check the log.\n", listen)
	}
	return nil
}

func runUninstall() error {
	if runtime.GOOS != "darwin" {
		return errors.New("uninstall is supported on macOS only")
	}
	plistPath, _, err := launchdPaths()
	if err != nil {
		return err
	}
	launchctl("bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchdLabel))
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Printf("Removed the background bridge. Your proxy list stays in %s.\n", configDir())
	return nil
}

// flagValue finds -name value, --name value, -name=value or --name=value in args.
func flagValue(args []string, name, def string) string {
	for i, a := range args {
		a = strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-")
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v
		}
	}
	return def
}
