// claude-codex — one command that lets a developer run OpenAI Codex on their
// own Claude Code login, with no API key. It starts a localhost OpenAI-compatible
// bridge in-process (bridge.go) for the lifetime of one command, then launches
// the real Codex CLI pointed at it via `-c` flags — writing nothing to the
// user's Codex config. Each person uses their own ~/.claude session.
//
// Consent-first: on first run it discloses exactly what it does and waits for a
// yes; installing software (the Codex CLI, if missing) is always a separate,
// explicit opt-in that defaults to no.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const version = "0.1.0"

func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

func consentMarkerPath() string { return filepath.Join(homeDir(), ".claude-codex", "consent") }
func claudeDirPath() string     { return filepath.Join(homeDir(), ".claude") }

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "claude-codex: "+format+"\n", a...)
	os.Exit(1)
}

func main() {
	dryRun := flag.Bool("dry-run", false, "print what would happen and exit, writing/installing nothing")
	what := flag.Bool("what", false, "show current state (claude login, codex, consent) and exit")
	uninstall := flag.Bool("uninstall", false, "remove the consent marker and print cleanup steps")
	assumeYes := flag.Bool("yes", false, "pre-approve the first-run consent (for reviewed/managed rollout)")
	quiet := flag.Bool("quiet", false, "no banner, colour, or animation (also auto-off when piped)")
	demoMode := flag.Bool("demo", false, "play the animated startup sequence and exit (no work, no Codex)")
	pick := flag.Bool("pick", false, "force the model menu (it already shows by default on a terminal)")
	showVersion := flag.Bool("version", false, "print version and exit")
	var model string
	flag.StringVar(&model, "model", defaultModel, "Claude model: opus | sonnet | haiku, or a full claude-* id")
	flag.StringVar(&model, "m", defaultModel, "shorthand for --model")
	flag.Parse()
	passthrough := flag.Args()
	model = normalizeModel(model)

	// Did the user pin a model explicitly? If so, skip the interactive menu.
	modelExplicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "m" || f.Name == "model" {
			modelExplicit = true
		}
	})

	initUI(*quiet)

	if *demoMode {
		demo()
		return
	}

	// The embedded bridge logs each request via the standard logger; keep that
	// out of the user's terminal unless they ask for it.
	if os.Getenv("CLAUDE_CODEX_DEBUG") == "" {
		log.SetOutput(io.Discard)
	}

	switch {
	case *showVersion:
		fmt.Println("claude-codex", version)
		return
	case *what:
		cmdWhat()
		return
	case *uninstall:
		cmdUninstall()
		return
	}

	banner()

	// Preflight: the user's own Claude login must exist.
	sp := newSpinner("Checking your Claude login…")
	sp.Start()
	if err := checkClaudeReady(); err != nil {
		sp.Stop("fail", "Claude login not found")
		fmt.Println()
		fail("%v", err)
	}
	sp.Stop("ok", "Claude login found "+dim("(~/.claude)"))

	// First-run consent (skipped with --yes or once already granted).
	if !*assumeYes && !hasConsent() {
		if !askConsent() {
			fmt.Println("\n  " + yellow("✗") + " Aborted — nothing was changed on your system.")
			return
		}
	}

	if *dryRun {
		printPlan()
		return
	}

	codexBin, err := ensureCodex(*assumeYes)
	if err != nil {
		fail("%v", err)
	}
	step("ok", "Codex CLI ready")

	recordConsent()

	sp = newSpinner("Starting local bridge…")
	sp.Start()
	port, err := startEphemeralBridge()
	if err != nil {
		sp.Stop("fail", "bridge failed to start")
		fail("could not start the local bridge: %v", err)
	}
	sp.Stop("ok", fmt.Sprintf("Bridge ready %s", dim(fmt.Sprintf("(127.0.0.1:%d)", port))))

	// Select the Claude model. The interactive menu is the default on a real
	// terminal; it is skipped when a model was pinned with -m, with --quiet, or
	// when not interactive (piped/CI) — then the flag value or default is used.
	if *pick || (!modelExplicit && !*quiet) {
		if chosen, ok := pickModel(model); ok {
			model = chosen
		}
	}
	step("ok", "Model "+bold(model))

	handoff(port)
	runCodex(codexBin, port, model, passthrough)
}

// normalizeModel accepts the common tier names (any case) and full claude-* ids,
// defaulting to the built-in default for anything unrecognized.
func normalizeModel(m string) string {
	s := strings.ToLower(strings.TrimSpace(m))
	switch {
	case s == "opus" || s == "sonnet" || s == "haiku":
		return s
	case strings.HasPrefix(s, "claude-"):
		return strings.TrimSpace(m)
	case strings.Contains(s, "opus"):
		return "opus"
	case strings.Contains(s, "haiku"):
		return "haiku"
	case strings.Contains(s, "sonnet"):
		return "sonnet"
	default:
		return defaultModel
	}
}

// ---- preflight -------------------------------------------------------------

func checkClaudeReady() error {
	if _, err := exec.LookPath(claudeBin); err != nil {
		return fmt.Errorf("the `claude` CLI is not installed or not on PATH.\n" +
			"Install Claude Code and run `claude` once to log in, then try again.")
	}
	if _, err := os.Stat(claudeDirPath()); err != nil {
		return fmt.Errorf("no ~/.claude found — you are not logged in to Claude Code.\n" +
			"Run `claude` once to log in, then try again.")
	}
	return nil
}

// ---- consent ---------------------------------------------------------------

func hasConsent() bool {
	_, err := os.Stat(consentMarkerPath())
	return err == nil
}

func recordConsent() {
	dir := filepath.Dir(consentMarkerPath())
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(consentMarkerPath(), []byte(fmt.Sprintf("granted %s\n", time.Now().Format(time.RFC3339))), 0o644)
}

func disclosure() string {
	return `claude-codex runs OpenAI Codex on YOUR Claude login. Before anything runs,
here is exactly what it does on this machine:

  1. RUN   a local bridge on 127.0.0.1 (a random free port) only while a
           Codex command is running. It stops the moment the command exits —
           nothing stays running in the background.
  2. USE   your existing Claude login (~/.claude), by launching the claude
           CLI. No token is copied, stored, or sent anywhere external.
  3. PASS  the bridge address to Codex on its command line (-c flags).
           It does NOT edit your ~/.codex/config.toml.
  4. SAVE  a tiny marker at ~/.claude-codex/consent so it won't ask again.

If the Codex CLI itself is missing, it will ask you SEPARATELY before
installing anything — and never installs system-wide without a yes.

Nothing above is written or installed until you type y.`
}

func askConsent() bool {
	fmt.Println()
	fmt.Println("  " + bold("First run — your permission is needed."))
	fmt.Println()
	fmt.Println(disclosure())
	fmt.Print("\n  " + cyan("Proceed?") + " " + dim("[y/N]") + " ")
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes"
}

func printPlan() {
	fmt.Println("DRY RUN — the following WOULD happen (nothing was changed):")
	fmt.Println(disclosure())
	fmt.Println()
	codexBin, _ := findCodex()
	if codexBin == "" {
		fmt.Println("  • Codex CLI: NOT found — you would be asked whether to run `npm install -g @openai/codex`.")
	} else {
		fmt.Printf("  • Codex CLI: found at %s (no install needed).\n", codexBin)
	}
	fmt.Printf("  • Consent marker: %s (would be created).\n", consentMarkerPath())
}

// ---- codex discovery / install --------------------------------------------

// findCodex returns the path to a usable Codex CLI: the real `codex` on PATH
// first, else the binary bundled with the VS Code Codex extension.
func findCodex() (string, string) {
	// Prefer the official binary bundled with the VS Code Codex extension.
	matches, _ := filepath.Glob(filepath.Join(homeDir(), ".vscode/extensions/openai.chatgpt-*/bin/linux-x86_64/codex"))
	sort.Strings(matches)
	if len(matches) > 0 {
		return matches[len(matches)-1], "vscode-extension"
	}
	// Otherwise a `codex` on PATH — but never the unofficial snap build, which
	// lacks the flags this tool needs.
	if p, err := exec.LookPath("codex"); err == nil && !isSnapPath(p) {
		return p, "path"
	}
	return "", ""
}

func isSnapPath(p string) bool {
	if strings.Contains(p, "/snap/") {
		return true
	}
	if target, err := filepath.EvalSymlinks(p); err == nil && strings.Contains(target, "/snap/") {
		return true
	}
	return false
}

func ensureCodex(assumeYes bool) (string, error) {
	if bin, _ := findCodex(); bin != "" {
		return bin, nil
	}
	fmt.Println("The Codex CLI is not installed.")
	fmt.Println("  Command to install it: npm install -g @openai/codex")
	install := assumeYes
	if !assumeYes {
		fmt.Print("Install it now with that command? [y/N] ")
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		ans := strings.ToLower(strings.TrimSpace(line))
		install = ans == "y" || ans == "yes"
	}
	if !install {
		return "", fmt.Errorf("Codex CLI not installed. Install it yourself with:\n  npm install -g @openai/codex\nthen re-run claude-codex.")
	}
	if _, err := exec.LookPath("npm"); err != nil {
		return "", fmt.Errorf("npm is not installed, so Codex can't be auto-installed. Install Node.js/npm, then `npm install -g @openai/codex`.")
	}
	fmt.Println("Installing Codex CLI (npm install -g @openai/codex)…")
	cmd := exec.Command("npm", "install", "-g", "@openai/codex")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("codex install failed: %w", err)
	}
	bin, _ := findCodex()
	if bin == "" {
		return "", fmt.Errorf("codex still not found after install; check that npm's global bin is on your PATH")
	}
	return bin, nil
}

// ---- bridge + codex launch -------------------------------------------------

// startEphemeralBridge binds 127.0.0.1 on a random free port, serves the bridge
// on it in a goroutine, waits until it answers /health, and returns the port.
// The server is torn down when the process exits.
func startEphemeralBridge() (int, error) {
	if err := initWorkdir(); err != nil {
		return 0, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go func() { _ = http.Serve(ln, bridgeHandler("")) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
		if err == nil {
			resp.Body.Close()
			return port, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0, fmt.Errorf("bridge did not become healthy in time")
}

// runCodex execs the Codex CLI pointed at the in-process bridge entirely via -c
// overrides, so ~/.codex/config.toml is never touched. A dummy key is injected
// for the subprocess; the local bridge ignores auth.
func runCodex(codexBin string, port int, model string, passthrough []string) {
	base := fmt.Sprintf("http://127.0.0.1:%d/v1", port)
	overrides := []string{
		"-c", "model_provider=claude_bridge",
		"-c", "model=" + model,
		"-c", "model_providers.claude_bridge.name=Claude Bridge",
		"-c", "model_providers.claude_bridge.base_url=" + base,
		"-c", "model_providers.claude_bridge.env_key=CLAUDE_BRIDGE_KEY",
		"-c", "model_providers.claude_bridge.wire_api=responses",
	}

	args := append([]string{}, overrides...)
	args = append(args, "--no-daemon")

	// Decide subcommand: a bare prompt (no recognized subcommand) -> `exec`.
	rest := passthrough
	if len(rest) > 0 && !isCodexSubcommand(rest[0]) && !strings.HasPrefix(rest[0], "-") {
		args = append(args, "exec", "--skip-git-repo-check")
		args = append(args, rest...)
	} else {
		args = append(args, rest...)
	}

	cmd := exec.Command(codexBin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "CLAUDE_BRIDGE_KEY=local")
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		fail("failed to run codex: %v", err)
	}
}

func isCodexSubcommand(s string) bool {
	switch s {
	case "exec", "e", "review", "login", "logout", "mcp", "resume", "apply", "a",
		"agents", "app-server", "sandbox", "completion", "update", "doctor", "queue":
		return true
	}
	return false
}

// ---- --what / --uninstall --------------------------------------------------

func cmdWhat() {
	fmt.Println("claude-codex state:")
	if _, err := exec.LookPath(claudeBin); err == nil {
		fmt.Println("  • claude CLI:   found on PATH")
	} else {
		fmt.Println("  • claude CLI:   NOT found")
	}
	if _, err := os.Stat(claudeDirPath()); err == nil {
		fmt.Println("  • claude login: ~/.claude present")
	} else {
		fmt.Println("  • claude login: ~/.claude MISSING (run `claude` to log in)")
	}
	if bin, src := findCodex(); bin != "" {
		fmt.Printf("  • codex CLI:    %s (%s)\n", bin, src)
	} else {
		fmt.Println("  • codex CLI:    NOT found (would offer to install)")
	}
	if hasConsent() {
		fmt.Printf("  • consent:      granted (%s)\n", consentMarkerPath())
	} else {
		fmt.Println("  • consent:      not yet granted (will ask on first run)")
	}
	fmt.Println("  • config.toml:  NOT modified by this tool (provider passed via -c at launch)")
	fmt.Printf("  • models:       opus | sonnet | haiku (or a full claude-* id); default %s\n", defaultModel)
	fmt.Println("                  pick with:  claude-codex -m opus \"…\"")
}

func cmdUninstall() {
	removed := false
	if hasConsent() {
		if err := os.Remove(consentMarkerPath()); err == nil {
			removed = true
		}
	}
	dir := filepath.Dir(consentMarkerPath())
	_ = os.Remove(dir) // only succeeds if empty
	if removed {
		fmt.Println("Removed consent marker.")
	} else {
		fmt.Println("No consent marker to remove.")
	}
	fmt.Println("claude-codex never edited your ~/.codex/config.toml, so nothing there needs reverting.")
	fmt.Println("To finish removal, delete the binary:  rm", selfPath())
}

func selfPath() string {
	p, err := os.Executable()
	if err != nil {
		return "<path-to-claude-codex>"
	}
	return p
}
