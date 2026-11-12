// picker.go — an interactive arrow-key model selector. Linux raw-mode via
// syscall (our only target OS), no external dependencies. Falls back to the
// current model when stdin/stdout is not a TTY.
package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

type modelOption struct {
	id, label, desc string
}

var modelOptions = []modelOption{
	{"sonnet", "Sonnet", "balanced · default"},
	{"opus", "Opus", "most capable"},
	{"haiku", "Haiku", "fastest"},
}

func ioctlTermios(fd int, req uintptr, t *syscall.Termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(unsafe.Pointer(t)))
	if errno != 0 {
		return errno
	}
	return nil
}

func makeRaw(fd int) (*syscall.Termios, error) {
	var old syscall.Termios
	if err := ioctlTermios(fd, syscall.TCGETS, &old); err != nil {
		return nil, err
	}
	raw := old
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.ISIG
	raw.Iflag &^= syscall.IXON | syscall.ICRNL
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if err := ioctlTermios(fd, syscall.TCSETS, &raw); err != nil {
		return nil, err
	}
	return &old, nil
}

func restoreTermios(fd int, old *syscall.Termios) {
	_ = ioctlTermios(fd, syscall.TCSETS, old)
}

// pickModel shows the menu and returns the chosen model id. The bool is false
// if the pick was cancelled or the terminal is not interactive (caller keeps
// its current model).
func pickModel(current string) (string, bool) {
	if uiPlain || !isTTY(os.Stdin) || !isTTY(os.Stdout) {
		return current, false
	}
	sel := 0
	for i, o := range modelOptions {
		if o.id == current {
			sel = i
		}
	}
	fd := int(os.Stdin.Fd())
	old, err := makeRaw(fd)
	if err != nil {
		return current, false
	}
	defer restoreTermios(fd, old)

	fmt.Print("\033[?25l")       // hide cursor
	defer fmt.Print("\033[?25h") // show cursor on exit

	draw := func(first bool) {
		if !first {
			fmt.Printf("\033[%dA", len(modelOptions)+1) // move cursor up to redraw
		}
		fmt.Print("\r\033[J") // clear from cursor down
		fmt.Printf("  %s %s\n", bold("Select a Claude model"), dim("↑/↓ · Enter · q to keep default"))
		for i, o := range modelOptions {
			if i == sel {
				fmt.Printf("  %s %s  %s\n", cyan("❯"), bold(cyan(o.label)), dim(o.desc))
			} else {
				fmt.Printf("    %s  %s\n", o.label, dim(o.desc))
			}
		}
	}

	draw(true)
	buf := make([]byte, 3)
	for {
		n, err := os.Stdin.Read(buf[:1])
		if err != nil || n == 0 {
			return current, false
		}
		switch buf[0] {
		case '\r', '\n': // confirm
			return modelOptions[sel].id, true
		case 'q', 3: // q or Ctrl-C: cancel, keep current
			return current, false
		case 'k': // vim up
			sel = (sel - 1 + len(modelOptions)) % len(modelOptions)
			draw(false)
		case 'j': // vim down
			sel = (sel + 1) % len(modelOptions)
			draw(false)
		case '1', '2', '3':
			idx := int(buf[0] - '1')
			if idx < len(modelOptions) {
				return modelOptions[idx].id, true
			}
		case 0x1b: // ESC — maybe an arrow sequence
			m, _ := os.Stdin.Read(buf[1:3])
			if m >= 2 && buf[1] == '[' {
				switch buf[2] {
				case 'A': // up
					sel = (sel - 1 + len(modelOptions)) % len(modelOptions)
					draw(false)
				case 'B': // down
					sel = (sel + 1) % len(modelOptions)
					draw(false)
				}
			} else {
				return current, false // lone ESC cancels
			}
		}
	}
}
