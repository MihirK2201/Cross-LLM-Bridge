// ui.go — the terminal experience: a branded banner, colour, and a braille
// spinner for the real startup work (preflight + bridge warmup). Everything
// degrades automatically: no colour/animation when stdout is not a TTY, when
// NO_COLOR is set, or when --quiet is passed, so piped and scripted use stays
// clean. No external dependencies.
package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

var uiPlain bool // set true to force plain output (not a TTY, NO_COLOR, or --quiet)

func initUI(quiet bool) {
	uiPlain = quiet || os.Getenv("NO_COLOR") != "" || !isTTY(os.Stdout)
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// ANSI helpers — no-ops in plain mode.
const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiCyan   = "\033[36m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiRed    = "\033[31m"
	ansiMag    = "\033[35m"
	ansiBlue   = "\033[34m"
	ansiGray   = "\033[90m"
)

func paint(code, s string) string {
	if uiPlain {
		return s
	}
	return code + s + ansiReset
}

func bold(s string) string   { return paint(ansiBold, s) }
func dim(s string) string    { return paint(ansiDim, s) }
func cyan(s string) string   { return paint(ansiCyan, s) }
func green(s string) string  { return paint(ansiGreen, s) }
func yellow(s string) string { return paint(ansiYellow, s) }
func red(s string) string    { return paint(ansiRed, s) }
func mag(s string) string    { return paint(ansiMag, s) }
func gray(s string) string   { return paint(ansiGray, s) }

// banner prints the branded header. Compact so it is fine on every run.
func banner() {
	if uiPlain {
		fmt.Println("claude-codex — Codex, powered by your Claude login")
		return
	}
	glyph := mag("◆")
	name := bold(cyan("claude")) + bold("-") + bold(mag("codex"))
	tag := dim("Codex, powered by your Claude login")
	bar := gray("│")
	fmt.Println()
	fmt.Printf("  %s  %s\n", glyph, name)
	fmt.Printf("  %s  %s\n", bar, tag)
	fmt.Println()
}

// step prints a resolved check line: green ✓ / red ✗ / yellow •.
func step(state, msg string) {
	var mark string
	switch state {
	case "ok":
		mark = green("✓")
	case "fail":
		mark = red("✗")
	case "skip":
		mark = yellow("•")
	default:
		mark = gray("·")
	}
	fmt.Printf("  %s %s\n", mark, msg)
}

// spinner animates a single line while real work happens, then resolves it to a
// step. In plain mode it prints the start message once and the result once, with
// no animation or cursor tricks.
type spinner struct {
	msg     string
	frms    []string
	stop    chan struct{}
	done    sync.WaitGroup
	plain   bool
	started time.Time
}

// minSpin keeps a spinner on screen long enough to be seen even when the work
// it covers finishes in microseconds (login check, bridge warmup). It is a
// perception floor, not a fake delay — if the work takes longer, no time is added.
const minSpin = 650 * time.Millisecond

func newSpinner(msg string) *spinner {
	return &spinner{
		msg:   msg,
		frms:  []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
		stop:  make(chan struct{}),
		plain: uiPlain,
	}
}

func (s *spinner) Start() {
	s.started = time.Now()
	if s.plain {
		fmt.Printf("  … %s\n", s.msg)
		return
	}
	fmt.Print("\033[?25l") // hide cursor
	s.done.Add(1)
	go func() {
		defer s.done.Done()
		i := 0
		for {
			select {
			case <-s.stop:
				return
			default:
				frame := cyan(s.frms[i%len(s.frms)])
				fmt.Printf("\r  %s %s", frame, s.msg)
				i++
				time.Sleep(70 * time.Millisecond)
			}
		}
	}()
}

// Stop clears the spinner line and prints the resolved step. It keeps the
// animation up until at least minSpin has elapsed so it is actually visible.
func (s *spinner) Stop(state, msg string) {
	if s.plain {
		step(state, msg)
		return
	}
	if elapsed := time.Since(s.started); elapsed < minSpin {
		time.Sleep(minSpin - elapsed)
	}
	close(s.stop)
	s.done.Wait()
	fmt.Print("\r\033[K")  // clear the spinner line
	fmt.Print("\033[?25h") // show cursor
	step(state, msg)
}

// handoff prints the line shown right before Codex takes over the terminal.
func handoff(port int) {
	if uiPlain {
		fmt.Printf("Launching Codex (bridge on 127.0.0.1:%d)…\n", port)
		return
	}
	fmt.Printf("  %s %s %s\n\n",
		mag("▸"),
		bold("Launching Codex"),
		dim(fmt.Sprintf("· bridge on 127.0.0.1:%d · using your Claude login", port)))
}

// demo plays the full animated sequence with no real work, so you can see
// (and verify your terminal renders) the spinners, colours, and steps.
func demo() {
	banner()
	seq := []struct {
		msg, state, done string
		dur              time.Duration
	}{
		{"Checking your Claude login…", "ok", "Claude login found " + dim("(~/.claude)"), 1200 * time.Millisecond},
		{"Looking for the Codex CLI…", "ok", "Codex CLI ready", 1000 * time.Millisecond},
		{"Starting local bridge…", "ok", "Bridge ready " + dim("(127.0.0.1:41481)"), 1400 * time.Millisecond},
		{"Waking Claude…", "ok", "Claude is awake", 1600 * time.Millisecond},
	}
	for _, s := range seq {
		sp := newSpinner(s.msg)
		sp.Start()
		time.Sleep(s.dur)
		sp.Stop(s.state, s.done)
	}
	handoff(41481)
	if uiPlain {
		fmt.Println("(demo — plain mode: your terminal is not a TTY or NO_COLOR is set)")
	} else {
		fmt.Println("  " + green("✓") + " " + bold("That is the animation.") + dim(" Real runs spin only while the work actually takes."))
	}
}

// consentBox renders the disclosure inside a coloured frame.
func consentBox(body string) {
	if uiPlain {
		fmt.Println(body)
		return
	}
	width := 68
	top := "  " + gray("╭"+strings.Repeat("─", width)+"╮")
	bot := "  " + gray("╰"+strings.Repeat("─", width)+"╯")
	fmt.Println(top)
	for _, line := range strings.Split(body, "\n") {
		// pad/truncate to frame width (visual length approx; good enough for ASCII)
		vis := line
		if len(vis) > width-2 {
			vis = vis[:width-2]
		}
		fmt.Printf("  %s %-*s %s\n", gray("│"), width-2, vis, gray("│"))
	}
	fmt.Println(bot)
}
