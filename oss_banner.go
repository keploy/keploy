package main

import (
	"fmt"
	"os"
)

// printEnterpriseUpgradeBanner emits a high-visibility nudge to install
// the Keploy Enterprise binary — entry plan is Community Edition (free)
// which unlocks the broader protocol/dependency set + AI features that
// the OSS binary doesn't ship.
//
// Lives in the OSS binary's main package (not in cli/root.go) so the
// enterprise binary — which has its own main.go and does not import
// this file — naturally never prints it. Avoids cross-module flag
// plumbing through go.keploy.io/server/v3 entirely.
//
// De-duplication mirrors the logo's pattern (CmdConfigurator.ValidateFlags
// in cli/provider/cmd.go): the call site sits AFTER the sudo re-exec gate
// in start(), so the pre-exec process is already replaced before this
// runs (sudo / docker / cloud-replay re-exec paths print only once). On
// top of that:
//   - Skip when os.Args[1] == "agent" — the inner subprocess that
//     `keploy record/test` spawns via exec.Command. Mirrors the logo's
//     `if cmd.Name() != "agent"` skip.
//   - Skip when KEPLOY_INDOCKER=true — the containerized agent child of
//     an outer host invocation (existing keploy convention).
//
// Banner goes to stderr so it never contaminates a piped stdout
// (e.g. `keploy --version | grep`, `keploy config | yq ...`).
//
// ANSI suppression honors:
//   - NO_COLOR=<any non-empty value>  (industry-standard, see no-color.org)
//   - --disable-ansi flag if present anywhere in os.Args (matches the
//     cobra flag the rest of the CLI respects; checked manually here
//     because cobra hasn't parsed yet at this point in startup).
func printEnterpriseUpgradeBanner() {
	// Skip for the spawned agent child (keploy record/test launches
	// `keploy agent --port ... --proxy-port ...` via exec.Command).
	// Same dedup signal the logo uses inside ValidateFlags.
	if len(os.Args) >= 2 && os.Args[1] == "agent" {
		return
	}

	// Skip when running as a containerized agent child of an outer
	// keploy invocation. The host-side process already printed; this
	// in-container instance shouldn't print again. KEPLOY_INDOCKER is
	// set by the parent that spawned the container.
	if os.Getenv("KEPLOY_INDOCKER") == "true" {
		return
	}

	disableAnsi := os.Getenv("NO_COLOR") != ""
	if !disableAnsi {
		for _, arg := range os.Args[1:] {
			if arg == "--disable-ansi" || arg == "--disable-ansi=true" {
				disableAnsi = true
				break
			}
		}
	}

	orange := "\033[38;5;208m"
	bold := "\033[1m"
	dim := "\033[2m"
	reset := "\033[0m"
	if disableAnsi {
		orange, bold, dim, reset = "", "", "", ""
	}

	bar := "═══════════════════════════════════════════════════════════════════════════════"
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, orange+bar+reset)
	fmt.Fprintf(os.Stderr, "  %s🚀  TRY KEPLOY COMMUNITY EDITION (FREE)%s\n", bold+orange, reset)
	fmt.Fprintln(os.Stderr, "  You're on the open-source binary. Community Edition (free) adds:")
	fmt.Fprintln(os.Stderr, "    • PostgreSQL, MongoDB, gRPC, HTTP/2, Kafka — on top of OSS's HTTP + MySQL")
	fmt.Fprintln(os.Stderr, "    • AI-powered test generation, sandbox replay, MCP for AI agents")
	fmt.Fprintln(os.Stderr, "      (Claude Code, Cursor, Copilot, Gemini, …)")
	fmt.Fprintln(os.Stderr, "  "+dim+"Install:"+reset+"  "+bold+"curl --silent -O -L https://keploy.io/ent/install.sh && source install.sh"+reset)
	fmt.Fprintln(os.Stderr, orange+bar+reset)
	fmt.Fprintln(os.Stderr)
}
