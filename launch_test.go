package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestProxyEnv(t *testing.T) {
	env := proxyEnv("127.0.0.1:1082")
	want := map[string]string{
		"HTTP_PROXY":         "http://127.0.0.1:1082",
		"HTTPS_PROXY":        "http://127.0.0.1:1082",
		"http_proxy":         "http://127.0.0.1:1082",
		"https_proxy":        "http://127.0.0.1:1082",
		"NO_PROXY":           "localhost,127.0.0.1,::1",
		"no_proxy":           "localhost,127.0.0.1,::1",
		"NODE_USE_ENV_PROXY": "1",
		"CODEP":              "1",
	}
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("env %v", got)
	}
}

func TestOpenArgsSeparate(t *testing.T) {
	args := openArgs(launchOpts{
		App: "Visual Studio Code", SocksAddr: "127.0.0.1:1080", HTTPAddr: "127.0.0.1:1082",
		DataDir: "/data/vscode", Paths: []string{"/work/proj"},
	})
	s := strings.Join(args, " ")
	for _, want := range []string{
		"-n -a Visual Studio Code ",
		"--env HTTPS_PROXY=http://127.0.0.1:1082 ",
		"--args --user-data-dir=/data/vscode --proxy-server=socks5://127.0.0.1:1080 /work/proj",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in %q", want, s)
		}
	}
	if !strings.HasSuffix(s, "/work/proj") {
		t.Fatalf("paths must come last: %q", s)
	}
}

func TestOpenArgsAll(t *testing.T) {
	args := openArgs(launchOpts{App: "Cursor", All: true, SocksAddr: "127.0.0.1:1080", HTTPAddr: "127.0.0.1:1082", DataDir: "/ignored"})
	s := strings.Join(args, " ")
	if strings.Contains(s, "-n ") || strings.Contains(s, "user-data-dir") {
		t.Fatalf("--all must start the regular instance: %q", s)
	}
	if !strings.HasPrefix(s, "-a Cursor ") || !strings.HasSuffix(s, "--args --proxy-server=socks5://127.0.0.1:1080") {
		t.Fatalf("%q", s)
	}
}

func TestLinkSettings(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "User")
	os.WriteFile(filepath.Join(src, "settings.json"), []byte("{}"), 0o644)
	os.Mkdir(filepath.Join(src, "snippets"), 0o755)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	// an existing keybindings.json must not be replaced
	os.WriteFile(filepath.Join(src, "keybindings.json"), []byte("[]"), 0o644)
	os.WriteFile(filepath.Join(dst, "keybindings.json"), []byte("[1]"), 0o644)

	linked, err := linkSettings(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(linked, []string{"settings.json", "snippets"}) {
		t.Fatalf("linked %v", linked)
	}
	if target, _ := os.Readlink(filepath.Join(dst, "settings.json")); target != filepath.Join(src, "settings.json") {
		t.Fatalf("symlink -> %q", target)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "keybindings.json")); string(b) != "[1]" {
		t.Fatal("existing file was replaced")
	}
	// a second run changes nothing
	if again, err := linkSettings(src, dst); err != nil || len(again) != 0 {
		t.Fatalf("second run: %v %v", again, err)
	}
}

func TestAppSettingsDir(t *testing.T) {
	cases := map[string]string{
		"Visual Studio Code":            "Code",
		"Visual Studio Code - Insiders": "Code - Insiders",
		"Cursor":                        "Cursor",
		"Some Unknown Editor":           "",
	}
	for app, want := range cases {
		if got := appSupportName(app); got != want {
			t.Errorf("%s: %q, want %q", app, got, want)
		}
	}
	if got := slug("Visual Studio Code - Insiders"); got != "visual-studio-code-insiders" {
		t.Fatalf("slug %q", got)
	}
}

func TestLaunchdPlist(t *testing.T) {
	p := launchdPlist("io.github.codep", []string{"/usr/local/bin/codep", "serve", "-proxies", "/a & b/proxies.txt"}, "/logs/codep.log")
	for _, want := range []string{
		"<string>io.github.codep</string>",
		"<string>/usr/local/bin/codep</string>",
		"<string>/a &amp; b/proxies.txt</string>",
		"<key>KeepAlive</key>",
		"<string>/logs/codep.log</string>",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %q in\n%s", want, p)
		}
	}
}

func TestConfigDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if got := configDir(); got != "/xdg/codep" {
		t.Fatalf("%q", got)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	home, _ := os.UserHomeDir()
	if got := configDir(); got != filepath.Join(home, ".config", "codep") {
		t.Fatalf("%q", got)
	}
}

func TestLaunchEnvDropsEditorVars(t *testing.T) {
	in := []string{"HOME=/h", "PATH=/bin", "ELECTRON_RUN_AS_NODE=1", "VSCODE_IPC_HOOK=/x.sock", "VSCODE_PID=1", "VSCODE_EXTENSIONS=/ext", "HTTPS_PROXY=http://other:1", "LANG=en"}
	got := launchEnv(in)
	if !reflect.DeepEqual(got, []string{"HOME=/h", "PATH=/bin", "VSCODE_EXTENSIONS=/ext", "LANG=en"}) {
		t.Fatalf("%v", got)
	}
}

func TestCheckDataDirLength(t *testing.T) {
	if err := checkDataDir("/Users/someone/Library/Application Support/codep/visual-studio-code"); err != nil {
		t.Fatal(err)
	}
	if err := checkDataDir("/" + strings.Repeat("a", 90)); err == nil {
		t.Fatal("expected a path length error")
	}
}

func TestFlagValue(t *testing.T) {
	args := []string{"-listen", "127.0.0.1:2080", "--dashboard=", "-type=http"}
	if flagValue(args, "listen", "d") != "127.0.0.1:2080" || flagValue(args, "dashboard", "d") != "" ||
		flagValue(args, "type", "d") != "http" || flagValue(args, "proxies", "d") != "d" {
		t.Fatal("flagValue")
	}
}

func TestLaunchdPlistIsValid(t *testing.T) {
	if _, err := exec.LookPath("plutil"); err != nil {
		t.Skip("plutil exists on macOS only")
	}
	f := filepath.Join(t.TempDir(), "x.plist")
	os.WriteFile(f, []byte(launchdPlist(launchdLabel, []string{"/bin/codep", "serve", "-proxies", "/a & <b>/p.txt"}, "/tmp/l.log")), 0o644)
	if out, err := exec.Command("plutil", "-lint", f).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
}

func TestShellConflicts(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, ".zshrc"), []byte("export A=1\n# alias codep='old'\nalias codep='osascript -e quit'\n"), 0o644)
	os.WriteFile(filepath.Join(home, ".bashrc"), []byte("codep() { echo hi; }\nalias codepx=1\n"), 0o644)
	os.MkdirAll(filepath.Join(home, ".config", "fish"), 0o755)
	os.WriteFile(filepath.Join(home, ".config", "fish", "config.fish"), []byte("alias codep 'echo'\n"), 0o644)
	got := shellConflicts(home)
	want := []string{
		filepath.Join(home, ".zshrc") + ":3",
		filepath.Join(home, ".bashrc") + ":1",
		filepath.Join(home, ".config", "fish", "config.fish") + ":1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if c := shellConflicts(t.TempDir()); len(c) != 0 {
		t.Fatalf("empty home: %v", c)
	}
}
