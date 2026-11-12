// codex-claude — the mirror of claude-codex. It runs CLAUDE CODE's harness (the
// `claude` CLI) on a GPT model served by your own Codex login: it starts a local
// Anthropic-Messages-API gateway (gateway.go) whose backend is the official
// `codex` CLI (backend_codex.go), then launches `claude` pointed at that gateway
// with an isolated config dir, so your real Claude login, settings and history
// are never used or touched. A scripted mock backend (backend_mock.go) is also
// available for testing the harness without any model.
//
// Consent-first: the first run discloses exactly what it does and where your
// data goes, and waits for a yes.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const version = "0.3.0"

type options struct {
	backend, model                       string
	yes, quiet, version, what, uninstall bool
	servePort                            int
	rest                                 []string // passed through to claude
}

// parseArgs consumes codex-claude's own flags from the front and passes
// everything from the first unrecognized argument (or "--") on to claude.
func parseArgs(args []string) (options, error) {
	o := options{backend: "codex"}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := a, "", false
		if strings.HasPrefix(a, "--") {
			if k, v, ok := strings.Cut(a, "="); ok {
				name, val, hasVal = k, v, true
			}
		}
		value := func() (string, error) {
			if hasVal {
				return val, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", name)
			}
			i++
			return args[i], nil
		}
		var err error
		switch name {
		case "--yes", "-y":
			o.yes = true
		case "--quiet":
			o.quiet = true
		case "--version":
			o.version = true
		case "--what":
			o.what = true
		case "--uninstall":
			o.uninstall = true
		case "--backend":
			o.backend, err = value()
		case "--model", "-m":
			o.model, err = value()
		case "--serve":
			var v string
			if v, err = value(); err == nil {
				o.servePort, err = strconv.Atoi(v)
			}
		case "--":
			o.rest = args[i+1:]
			return o, nil
		default:
			o.rest = args[i:]
			return o, nil
		}
		if err != nil {
			return o, err
		}
	}
	return o, nil
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "codex-claude: "+format+"\n", a...)
	os.Exit(1)
}

func homeDir() string { h, _ := os.UserHomeDir(); return h }

func stateDir() string          { return filepath.Join(homeDir(), ".codex-claude") }
func consentMarkerPath() string { return filepath.Join(stateDir(), "consent") }
func claudeHomePath() string    { return filepath.Join(stateDir(), "claude-home") }

func main() {
	o, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "codex-claude:", err)
		os.Exit(2)
	}
	initUI(o.quiet)
	if os.Getenv("CODEX_CLAUDE_DEBUG") == "" && o.servePort == 0 {
		log.SetOutput(io.Discard)
	}

	switch {
	case o.version:
		fmt.Println("codex-claude", version)
	case o.what:
		cmdWhat(o)
	case o.uninstall:
		cmdUninstall()
	case o.servePort > 0:
		runServe(o)
	default:
		run(o)
	}
}

// ---- backend selection -----------------------------------------------------

// buildBackend returns the backend, the model name Claude Code is told to use
// (its value only labels things; the backend decides the real model), and a
// human label for display.
func buildBackend(o options) (be backend, envName, label string, err error) {
	switch o.backend {
	case "codex":
		cb, err := newCodexBackend(o.model)
		if err != nil {
			return nil, "", "", err
		}
		if o.model != "" {
			return cb, o.model, o.model, nil
		}
		return cb, "codex", "your ChatGPT account's default model", nil
	case "mock":
		return mockBackend{}, "mock", "mock model", nil
	default:
		return nil, "", "", fmt.Errorf("unknown --backend %q (use codex or mock)", o.backend)
	}
}

// ---- main flow -------------------------------------------------------------

func run(o options) {
	banner()

	sp := newSpinner("Checking the Claude Code CLI…")
	sp.Start()
	claudeBin, err := exec.LookPath("claude")
	if err != nil {
		sp.Stop("fail", "Claude Code CLI not found")
		fail("install Claude Code first (npm install -g @anthropic-ai/claude-code), then try again.")
	}
	sp.Stop("ok", "Claude Code CLI found")

	be, envName, label, err := buildBackend(o)
	if err != nil {
		fail("%v", err)
	}

	sp = newSpinner("Checking your " + be.Name() + " login…")
	sp.Start()
	note := be.Name() + " backend ready"
	if pf, ok := be.(preflighter); ok {
		var perr error
		if note, perr = pf.Preflight(context.Background()); perr != nil {
			sp.Stop("fail", "backend check failed")
			fail("%v", perr)
		}
	}
	sp.Stop("ok", note)

	if !o.yes && !hasConsent() {
		if !askConsent(be) {
			fmt.Println("\n  " + yellow("✗") + " Aborted — nothing was changed on your system.")
			return
		}
	}
	recordConsent()

	// Show the real model, not a placeholder: ask codex which model it uses.
	modelShown := false
	if r, ok := be.(interface {
		ResolveModel(context.Context) (string, error)
	}); ok && o.model == "" {
		sp = newSpinner("Detecting your Codex model…")
		sp.Start()
		name, rerr := r.ResolveModel(context.Background())
		if rerr != nil {
			sp.Stop("fail", "could not get an answer from Codex")
			fail("%v", rerr)
		}
		if name != "" {
			sp.Stop("ok", "Model "+bold(name)+dim(" (your ChatGPT account's default)"))
			envName, label, modelShown = name, name, true
		} else {
			sp.Stop("ok", "Codex answered")
		}
	}

	home, err := ensureClaudeHome()
	if err != nil {
		fail("could not prepare the isolated Claude config: %v", err)
	}

	token := newToken()
	port, err := startGateway(&gateway{be: be, token: token})
	if err != nil {
		fail("could not start the gateway: %v", err)
	}
	step("ok", fmt.Sprintf("Gateway ready %s", dim(fmt.Sprintf("(127.0.0.1:%d)", port))))
	if !modelShown {
		step("ok", "Model "+bold(label))
	}

	handoff(port, label)
	runClaude(claudeBin, port, token, home, envName, o.rest)
}

func runServe(o options) {
	be, _, _, err := buildBackend(o)
	if err != nil {
		fail("%v", err)
	}
	token := os.Getenv("CODEX_CLAUDE_TOKEN")
	if token == "" {
		token = newToken()
	}
	addr := fmt.Sprintf("127.0.0.1:%d", o.servePort)
	fmt.Printf("serving %s backend on %s\ntoken: %s\n", be.Name(), addr, token)
	fail("%v", http.ListenAndServe(addr, (&gateway{be: be, token: token}).handler()))
}

func startGateway(gw *gateway) (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	srv := &http.Server{Handler: gw.handler()}
	go func() { _ = srv.Serve(ln) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
		if err == nil {
			resp.Body.Close()
			return port, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0, errors.New("gateway did not become healthy")
}

// ensureClaudeHome creates the isolated config dir Claude Code runs in, so the
// user's real ~/.claude (login, settings, history) is never used or modified.
func ensureClaudeHome() (string, error) {
	home := claudeHomePath()
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", err
	}
	cfg := filepath.Join(home, ".claude.json")
	if _, err := os.Stat(cfg); os.IsNotExist(err) {
		// Skip first-run onboarding; there is no login to set up here.
		if err := os.WriteFile(cfg, []byte("{\"hasCompletedOnboarding\": true, \"theme\": \"dark\"}\n"), 0o600); err != nil {
			return "", err
		}
	}
	return home, nil
}

func filterEnv(env []string, drop ...string) []string {
	skip := map[string]bool{}
	for _, d := range drop {
		skip[d] = true
	}
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if !skip[k] {
			out = append(out, kv)
		}
	}
	return out
}

func runClaude(claudeBin string, port int, token, home, modelName string, args []string) {
	// Drop anything that would route Claude Code somewhere else.
	env := filterEnv(os.Environ(),
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
		"ANTHROPIC_MODEL", "ANTHROPIC_SMALL_FAST_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL",
		"CLAUDE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX")
	env = append(env,
		fmt.Sprintf("ANTHROPIC_BASE_URL=http://127.0.0.1:%d", port),
		"ANTHROPIC_AUTH_TOKEN="+token,
		"CLAUDE_CONFIG_DIR="+home,
		"ANTHROPIC_MODEL="+modelName,
		"ANTHROPIC_DEFAULT_SONNET_MODEL="+modelName,
		"ANTHROPIC_DEFAULT_OPUS_MODEL="+modelName,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL="+modelName,
		"ANTHROPIC_SMALL_FAST_MODEL="+modelName,
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL=1", // else interactive mode pulls the plugin catalog from github.com
		"DISABLE_TELEMETRY=1",
		"DISABLE_ERROR_REPORTING=1",
		"DISABLE_AUTOUPDATER=1",
	)

	cmd := exec.Command(claudeBin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = env
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		fail("could not run claude: %v", err)
	}
}

// ---- consent ---------------------------------------------------------------

func hasConsent() bool { _, err := os.Stat(consentMarkerPath()); return err == nil }

func recordConsent() {
	_ = os.MkdirAll(stateDir(), 0o700)
	_ = os.WriteFile(consentMarkerPath(), []byte("granted "+time.Now().Format(time.RFC3339)+"\n"), 0o600)
}

func disclosure(be backend) string {
	where := "Nothing leaves this machine (mock model)."
	if be.Name() == "codex" {
		where = "Your prompts and the code/files Claude Code reads for a task are sent to\n" +
			"             OpenAI by the official codex CLI, under YOUR ChatGPT login (it counts\n" +
			"             against your plan's Codex usage; it also makes one tiny \"Reply with\n" +
			"             exactly: ok\" request at startup to learn the model name, cached for\n" +
			"             12 hours). This tool never reads or copies that login; codex uses it\n" +
			"             the way it always does."
	}
	return `codex-claude runs the Claude Code harness on a GPT model, using your Codex
login. Before anything runs, here is exactly what it does on this machine:

  1. RUN     a local gateway on 127.0.0.1 (random free port) only while the
             session runs; it stops when you exit.
  2. SEND    ` + where + `
  3. LAUNCH  the claude CLI pointed at the gateway, in an ISOLATED config dir
             (~/.codex-claude/claude-home). Your real Claude login, settings and
             history are not used or touched, and telemetry is turned off.
  4. SAVE    a marker at ~/.codex-claude/consent so it won't ask again.

Nothing above happens until you type y.`
}

func askConsent(be backend) bool {
	fmt.Println()
	fmt.Println("  " + bold("First run — your permission is needed."))
	fmt.Println()
	fmt.Println(disclosure(be))
	fmt.Print("\n  " + cyan("Proceed?") + " " + dim("[y/N]") + " ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes"
}

// ---- --what / --uninstall --------------------------------------------------

func cmdWhat(o options) {
	fmt.Println("codex-claude state:")
	if p, err := exec.LookPath("claude"); err == nil {
		fmt.Println("  • claude CLI:      " + p)
	} else {
		fmt.Println("  • claude CLI:      NOT found")
	}
	fmt.Println("  • backend:         " + o.backend)
	if o.backend == "codex" {
		if bin := findCodexBinary(); bin != "" {
			fmt.Println("  • codex CLI:       " + bin)
			out, _ := exec.Command(bin, "login", "status").CombinedOutput()
			fmt.Println("  • codex login:     " + strings.TrimSpace(string(out)))
		} else {
			fmt.Println("  • codex CLI:       NOT found")
		}
		if o.model != "" {
			fmt.Println("  • model:           " + o.model)
		} else {
			fmt.Println("  • model:           your ChatGPT account's default")
		}
	}
	if hasConsent() {
		fmt.Println("  • consent:         granted (" + consentMarkerPath() + ")")
	} else {
		fmt.Println("  • consent:         not yet granted (will ask on first run)")
	}
	fmt.Println("  • claude config:   " + claudeHomePath() + " (isolated; your ~/.claude is not used)")
}

func cmdUninstall() {
	if err := os.Remove(consentMarkerPath()); err == nil {
		fmt.Println("Removed consent marker.")
	} else {
		fmt.Println("No consent marker to remove.")
	}
	fmt.Println("codex-claude never edits your ~/.claude or ~/.codex.")
	fmt.Println("To remove everything it created:  rm -r " + stateDir())
	if p, err := os.Executable(); err == nil {
		fmt.Println("and delete the binary:            rm " + p)
	}
}
