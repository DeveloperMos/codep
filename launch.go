package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const restartCmd = "__restart"

type launchOpts struct {
	App       string
	All       bool // restart the regular instance with the proxy instead of opening a separate one
	SocksAddr string
	HTTPAddr  string
	DataDir   string
	Paths     []string
}

// proxyEnv is what terminals inside the proxied VS Code get. CLI tools read
// these variables; most of them speak HTTP proxy, not SOCKS5, so they point
// to the HTTP entry of the bridge.
func proxyEnv(httpAddr string) []string {
	u := "http://" + httpAddr
	local := "localhost,127.0.0.1,::1"
	return []string{
		"HTTP_PROXY=" + u, "HTTPS_PROXY=" + u,
		"http_proxy=" + u, "https_proxy=" + u,
		"NO_PROXY=" + local, "no_proxy=" + local,
		"NODE_USE_ENV_PROXY=1", // recent Node.js: fetch and http honor the variables above
		"CODEP=1",              // lets a shell prompt show that the proxy is on
	}
}

// openArgs builds arguments for macOS `open`. -n starts a new process even if
// the app is already running; with its own --user-data-dir it becomes a
// separate instance instead of handing over to the running one.
func openArgs(o launchOpts) []string {
	var args []string
	if !o.All {
		args = append(args, "-n")
	}
	args = append(args, "-a", o.App)
	for _, kv := range proxyEnv(o.HTTPAddr) {
		args = append(args, "--env", kv)
	}
	args = append(args, "--args")
	if !o.All {
		args = append(args, "--user-data-dir="+o.DataDir)
	}
	args = append(args, "--proxy-server=socks5://"+o.SocksAddr)
	return append(args, o.Paths...)
}

// linkSettings symlinks settings from the main profile into the separate
// instance, so both share settings, keybindings and snippets. Files that
// already exist in dst are left alone: delete a link and put a real file
// there to keep that setting separate.
func linkSettings(srcUser, dstUser string) ([]string, error) {
	var linked []string
	for _, name := range []string{"settings.json", "keybindings.json", "snippets", "tasks.json", "mcp.json"} {
		src, dst := filepath.Join(srcUser, name), filepath.Join(dstUser, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if _, err := os.Lstat(dst); err == nil {
			continue
		}
		if err := os.Symlink(src, dst); err != nil {
			return linked, err
		}
		linked = append(linked, name)
	}
	return linked, nil
}

// launchEnv is the environment for `open`, which passes it on to the app.
// Variables of the editor codep may be running in (ELECTRON_RUN_AS_NODE makes
// Electron start as plain Node and exit; VSCODE_* belong to the other
// instance) and proxy variables from the shell are dropped.
func launchEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case k == "VSCODE_EXTENSIONS", k == "VSCODE_PORTABLE":
			// set by the user on purpose, keep them
		case strings.HasPrefix(k, "ELECTRON_"), strings.HasPrefix(k, "VSCODE_"):
			continue
		case strings.EqualFold(k, "http_proxy"), strings.EqualFold(k, "https_proxy"),
			strings.EqualFold(k, "all_proxy"), strings.EqualFold(k, "no_proxy"):
			continue
		}
		out = append(out, kv)
	}
	return out
}

// VS Code puts its IPC socket (<version>-main.sock) into the data folder, and
// macOS limits unix socket paths to 103 bytes. With a longer path the app
// exits right after start without any message.
func checkDataDir(dir string) error {
	const maxSock, sockName = 103, "/1.100-main.sock"
	if len(dir)+len(sockName) > maxSock {
		return fmt.Errorf("--data-dir path is too long (%d chars): VS Code cannot create its socket there; use a path under %d chars", len(dir), maxSock-len(sockName))
	}
	return nil
}

func runOpenCmd(o launchOpts) error {
	cmd := exec.Command("open", openArgs(o)...)
	cmd.Env = launchEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("open %s: %v: %s", o.App, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func parseOpenFlags(name string, args []string) (launchOpts, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	all := fs.Bool("all", false, "restart the app itself with the proxy for every window")
	app := fs.String("app", "Visual Studio Code", "app to open: Visual Studio Code, Visual Studio Code - Insiders, Cursor, ...")
	socks := fs.String("socks", defaultSocks, "SOCKS5 address of the bridge")
	httpAddr := fs.String("http", defaultHTTP, "HTTP proxy address of the bridge, used by terminals")
	data := fs.String("data-dir", "", "state folder of the separate instance (default: one per app)")
	fs.Usage = func() { fmt.Fprintf(fs.Output(), usage, defaultProxiesPath()) }
	if err := fs.Parse(args); err != nil {
		return launchOpts{}, err
	}
	o := launchOpts{App: *app, All: *all, SocksAddr: *socks, HTTPAddr: *httpAddr, DataDir: *data}
	if o.DataDir == "" {
		o.DataDir = dataDir(*app)
	} else if abs, err := filepath.Abs(o.DataDir); err == nil {
		o.DataDir = abs
	}
	for _, p := range fs.Args() {
		abs, err := filepath.Abs(p)
		if err != nil {
			return o, err
		}
		o.Paths = append(o.Paths, abs)
	}
	return o, nil
}

func runOpen(args []string) error {
	o, err := parseOpenFlags("codep", args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if runtime.GOOS != "darwin" {
		return errors.New("opening VS Code is supported on macOS only for now; run `codep serve` and start VS Code with --proxy-server=socks5://" + o.SocksAddr)
	}
	if c, err := net.DialTimeout("tcp", o.SocksAddr, time.Second); err != nil {
		return fmt.Errorf("the bridge is not running on %s. Start it with `codep install` (background, starts at login) or `codep serve`", o.SocksAddr)
	} else {
		c.Close()
	}

	if o.All {
		// Quitting VS Code kills its terminals, including the one running
		// codep, so the restart happens in a detached process.
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		cmd := exec.Command(exe, append([]string{restartCmd}, args...)...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		cmd.Process.Release()
		fmt.Printf("Restarting %s with the proxy for all windows. If it asks about unsaved files, answer and it will continue.\n", o.App)
		return nil
	}

	if err := checkDataDir(o.DataDir); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(o.DataDir, "User"), 0o700); err != nil {
		return err
	}
	if name := appSupportName(o.App); name != "" {
		if base, err := os.UserConfigDir(); err == nil {
			linked, err := linkSettings(filepath.Join(base, name, "User"), filepath.Join(o.DataDir, "User"))
			if err != nil {
				return fmt.Errorf("share settings: %w", err)
			}
			if len(linked) > 0 {
				fmt.Printf("Shared with your main %s: %s\n", o.App, strings.Join(linked, ", "))
			}
		}
	}
	return runOpenCmd(o)
}

// runRestart quits the app, waits for it to exit and starts it again with
// the proxy. Runs detached, so errors go to a notification.
func runRestart(args []string) error {
	o, err := parseOpenFlags("codep", args)
	if err != nil {
		return err
	}
	o.All = true
	err = restartApp(o)
	if err != nil {
		notify(err.Error())
	}
	return err
}

func restartApp(o launchOpts) error {
	if appRunning(o.App) {
		exec.Command("osascript", "-e", fmt.Sprintf("quit app %q", o.App)).Run()
		// Leave time to answer "save changes?" dialogs.
		deadline := time.Now().Add(2 * time.Minute)
		for appRunning(o.App) {
			if time.Now().After(deadline) {
				return fmt.Errorf("%s did not quit within 2 minutes", o.App)
			}
			time.Sleep(300 * time.Millisecond)
		}
	}
	return runOpenCmd(o)
}

func appRunning(app string) bool {
	out, err := exec.Command("osascript", "-e", fmt.Sprintf("application %q is running", app)).Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func notify(msg string) {
	exec.Command("osascript", "-e", fmt.Sprintf("display notification %q with title %q", msg, "codep")).Run()
}

func runEnv(args []string) error {
	fs := flag.NewFlagSet("env", flag.ExitOnError)
	httpAddr := fs.String("http", defaultHTTP, "HTTP proxy address of the bridge")
	fs.Parse(args)
	for _, kv := range proxyEnv(*httpAddr) {
		k, v, _ := strings.Cut(kv, "=")
		fmt.Printf("export %s='%s'\n", k, v)
	}
	return nil
}
