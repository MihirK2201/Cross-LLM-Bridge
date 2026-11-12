// ui.go — the terminal experience: a branded banner, colour, and a braille
// spinner for the real startup work. Everything degrades automatically: no
// colour/animation when stdout is not a TTY, when NO_COLOR is set, or with
// --quiet, so piped and scripted use stays clean. No external dependencies.
package main

import (
	"fmt"
	"os"
	"sync"
	"time"
)

var uiPlain bool // true = plain output (not a TTY, NO_COLOR, or --quiet)

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

const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiCyan   = "\033[36m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiRed    = "\033[31m"
	ansiMag    = "\033[35m"
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

func banner() {
	if uiPlain {
		fmt.Println("codex-claude — the Claude Code harness on a GPT model")
		return
	}
	name := bold(cyan("codex")) + bold("-") + bold(mag("claude"))
	fmt.Println()
	fmt.Printf("  %s  %s\n", mag("◆"), name)
	fmt.Printf("  %s  %s\n", gray("│"), dim("the Claude Code harness on a GPT model"))
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

// spinner animates one line while real work happens, then resolves it to a
// step. In plain mode it prints the start message and the result once.
type spinner struct {
	msg     string
	frames  []string
	stop    chan struct{}
	done    sync.WaitGroup
	plain   bool
	started time.Time
}

// minSpin keeps a spinner on screen long enough to be seen when the work it
// covers finishes in microseconds. It is a perception floor, not a fake delay:
// slower work adds nothing.
const minSpin = 650 * time.Millisecond

func newSpinner(msg string) *spinner {
	return &spinner{
		msg:    msg,
		frames: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
		stop:   make(chan struct{}),
		plain:  uiPlain,
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
		for i := 0; ; i++ {
			select {
			case <-s.stop:
				return
			default:
				fmt.Printf("\r  %s %s", cyan(s.frames[i%len(s.frames)]), s.msg)
				time.Sleep(70 * time.Millisecond)
			}
		}
	}()
}

// Stop clears the spinner line and prints the resolved step.
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

// handoff prints the line shown right before Claude Code takes over the terminal.
func handoff(port int, model string) {
	if uiPlain {
		fmt.Printf("Launching Claude Code (gateway on 127.0.0.1:%d, model %s)…\n", port, model)
		return
	}
	fmt.Printf("  %s %s %s\n\n",
		mag("▸"),
		bold("Launching Claude Code"),
		dim(fmt.Sprintf("· gateway on 127.0.0.1:%d · model %s", port, model)))
}
