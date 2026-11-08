// codep runs VS Code (or a fork like Cursor) through a proxy that needs a
// login and password, with failover between several proxies.
package main

import (
	"fmt"
	"os"
)

var version = "dev"

const usage = `codep: run VS Code through an authenticated proxy

Usage:
  codep [--all] [--app NAME] [PATH...]   open VS Code through the proxy
  codep serve [flags]                    run the bridge in the foreground
  codep install [serve flags]            run the bridge in the background, start at login (macOS)
  codep uninstall                        stop the background bridge and remove it from login items
  codep env                              print proxy variables for other terminals: eval "$(codep env)"
  codep version

By default codep opens a separate VS Code window that goes through the proxy,
while your regular VS Code keeps working without it. With --all it restarts
VS Code itself with the proxy for every window.

Proxy list: %s
Dashboard:  http://127.0.0.1:1081
`

func main() {
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	var err error
	switch cmd {
	case "serve":
		err = runServe(args[1:])
	case "install":
		err = runInstall(args[1:])
	case "uninstall":
		err = runUninstall()
	case "env":
		err = runEnv(args[1:])
	case "version", "--version", "-v":
		fmt.Println("codep", version)
	case "help", "--help", "-h":
		fmt.Printf(usage, defaultProxiesPath())
	case restartCmd:
		err = runRestart(args[1:])
	default:
		err = runOpen(args)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "codep:", err)
		os.Exit(1)
	}
}
