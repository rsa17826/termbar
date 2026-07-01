package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// ── Constants ─────────────────────────────────────────────────────────────────

// Linux ioctl constants absent from syscall package on this Go version.
const (
	tcgets     uintptr = 0x5401
	tcsets     uintptr = 0x5402
	tiocgptn   uintptr = 0x80045430
	tiocsptlck uintptr = 0x40045431
)

// Winsize mirrors struct winsize (sys/ioctl.h).
// syscall.Winsize is not available in this Go version.
type Winsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

// ── PTY ───────────────────────────────────────────────────────────────────────

func openPTY() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}

	var ptsno uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
		master.Fd(), tiocgptn, uintptr(unsafe.Pointer(&ptsno))); errno != 0 {
		master.Close()
		return nil, nil, fmt.Errorf("TIOCGPTN: %w", errno)
	}

	var zero uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
		master.Fd(), tiocsptlck, uintptr(unsafe.Pointer(&zero))); errno != 0 {
		master.Close()
		return nil, nil, fmt.Errorf("TIOCSPTLCK: %w", errno)
	}

	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", ptsno),
		os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("open slave pts/%d: %w", ptsno, err)
	}

	return master, slave, nil
}

// ── Window size ───────────────────────────────────────────────────────────────

func getWinsize(fd uintptr) (*Winsize, error) {
	var ws Winsize
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
		fd, syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws))); errno != 0 {
		return nil, errno
	}
	return &ws, nil
}

func setWinsize(fd uintptr, ws *Winsize) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
		fd, syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(ws))); errno != 0 {
		return errno
	}
	return nil
}

// ── Raw mode ──────────────────────────────────────────────────────────────────

func makeRaw(fd int) (*syscall.Termios, error) {
	var t syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
		uintptr(fd), tcgets, uintptr(unsafe.Pointer(&t))); errno != 0 {
		return nil, errno
	}
	old := t

	t.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK |
		syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	t.Oflag &^= syscall.OPOST
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	t.Cflag &^= syscall.CSIZE | syscall.PARENB
	t.Cflag |= syscall.CS8
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
		uintptr(fd), tcsets, uintptr(unsafe.Pointer(&t))); errno != 0 {
		return nil, errno
	}
	return &old, nil
}

func restoreTermios(fd int, t *syscall.Termios) {
	syscall.Syscall(syscall.SYS_IOCTL,
		uintptr(fd), tcsets, uintptr(unsafe.Pointer(t)))
}

// ── State ─────────────────────────────────────────────────────────────────────

var (
	mu          sync.Mutex
	inAltScreen bool
	currentRows int
	statusFile  string
	// carry holds a trailing incomplete escape sequence from the previous
	// filterDECSTBM call so sequences split across buffer boundaries are caught.
	carry []byte
)

// ── Status bar ────────────────────────────────────────────────────────────────

func getStatus() string {
	b, err := os.ReadFile(statusFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// setScrollRegion restricts terminal scrolling to rows 1..(rows-1),
// leaving the last row permanently reserved for the status bar.
func setScrollRegion(rows int) {
	fmt.Fprintf(os.Stdout, "\033[1;%dr", rows-1)
}

// drawStatus saves cursor, jumps to the last row (outside the scroll region),
// writes status, then restores cursor. Caller must hold mu.
func drawStatus(rows int, status string) {
	if inAltScreen {
		return
	}
	fmt.Fprintf(os.Stdout,
		"\033[s\033[%d;1H\033[2K\033[33m%s\033[0m\033[u",
		rows, status,
	)
}

// rewriteDECSTBM rewrites a single DECSTBM parameter string so the bottom
// margin never reaches our reserved last row.
func rewriteDECSTBM(params string, rows int) string {
	top, bottom := 1, rows
	if params != "" {
		parts := strings.SplitN(params, ";", 2)
		if parts[0] != "" {
			if n, err := strconv.Atoi(parts[0]); err == nil {
				top = n
			}
		}
		if len(parts) == 2 && parts[1] != "" {
			if n, err := strconv.Atoi(parts[1]); err == nil {
				bottom = n
			}
		}
	}
	if bottom >= rows {
		bottom = rows - 1
	}
	return fmt.Sprintf("%d;%d", top, bottom)
}

// isPlainDecstbm reports whether params contains only digits and semicolons,
// i.e. it is a standard DECSTBM with no private-mode prefix such as '?'.
func isPlainDecstbm(params string) bool {
	for i := 0; i < len(params); i++ {
		if params[i] != ';' && (params[i] < '0' || params[i] > '9') {
			return false
		}
	}
	return true
}

// filterDECSTBM scans b for CSI DECSTBM sequences (\033[...r) and rewrites
// them so the bottom margin never reaches our reserved last row.
// A carry buffer handles sequences that are split across successive reads.
// Caller must hold mu.
func filterDECSTBM(b []byte, rows int) []byte {
	// Prepend any incomplete sequence left over from the previous call.
	if len(carry) > 0 {
		b = append(carry, b...)
		carry = nil
	}
	if !bytes.ContainsRune(b, 0x1b) {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		if b[i] != 0x1b {
			out = append(out, b[i])
			i++
			continue
		}
		// ESC at end of buffer: save and wait for next read.
		if i+1 >= len(b) {
			carry = []byte{0x1b}
			break
		}
		if b[i+1] != '[' {
			out = append(out, b[i])
			i++
			continue
		}
		// ESC [ found — scan the full CSI sequence.
		// Parameter bytes span 0x30-0x3F: digits, ';', and private-mode
		// markers '?', '<', '=', '>'. Scanning them all ensures sequences
		// like \033[?25h are never split mid-sequence at a buffer boundary.
		j := i + 2
		for j < len(b) && b[j] >= 0x30 && b[j] <= 0x3F {
			j++
		}
		// Intermediate bytes (0x20-0x2F) follow parameters in some sequences.
		for j < len(b) && b[j] >= 0x20 && b[j] <= 0x2F {
			j++
		}
		if j >= len(b) {
			// Incomplete CSI sequence — carry it to the next read.
			carry = append([]byte{}, b[i:]...)
			break
		}
		if b[j] == 'r' {
			// DECSTBM — rewrite bottom margin, but only for plain sequences
			// (digits and semicolons only; skip private-mode forms like \033[?...r).
			params := string(b[i+2 : j])
			if isPlainDecstbm(params) {
				out = append(out, '\033', '[')
				out = append(out, []byte(rewriteDECSTBM(params, rows))...)
				out = append(out, 'r')
				i = j + 1
				continue
			}
		}
		// Other CSI sequence — pass through unchanged.
		out = append(out, b[i:j+1]...)
		i = j + 1
	}
	return out
}

// writeOutput forwards PTY bytes to stdout and watches for escape sequences
// that would break our setup (alt-screen switches, full terminal reset).
func writeOutput(b []byte, rows int) {
	mu.Lock()
	defer mu.Unlock()

	altEnter := bytes.Contains(b, []byte("\033[?1049h")) ||
		bytes.Contains(b, []byte("\033[?47h"))
	altExit := bytes.Contains(b, []byte("\033[?1049l")) ||
		bytes.Contains(b, []byte("\033[?47l"))
	hasReset := bytes.Contains(b, []byte("\033c")) ||
		bytes.Contains(b, []byte("\033[!p"))

	if altEnter {
		inAltScreen = true
	}
	if altExit {
		inAltScreen = false
	}

	// Rewrite DECSTBM sequences so nothing can reclaim our last row.
	if !inAltScreen {
		b = filterDECSTBM(b, rows)
	}

	os.Stdout.Write(b)

	if hasReset || altExit {
		setScrollRegion(rows)
	}
	// always write to prevent flickering on pressing \n
	drawStatus(rows, getStatus())
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}

	statusFile = fmt.Sprintf("/tmp/termbar_%d", os.Getpid())

	master, slave, err := openPTY()
	if err != nil {
		fmt.Fprintln(os.Stderr, "termbar:", err)
		os.Exit(1)
	}

	ws, err := getWinsize(os.Stdin.Fd())
	if err != nil {
		fmt.Fprintln(os.Stderr, "termbar: get winsize:", err)
		os.Exit(1)
	}
	currentRows = int(ws.Row)

	// Child sees one fewer row — it can never write into the status row.
	if err := setWinsize(master.Fd(), &Winsize{
		Row: ws.Row - 1,
		Col: ws.Col,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "termbar: set child winsize:", err)
		os.Exit(1)
	}

	cmd := exec.Command(shell)
	cmd.Env = append(os.Environ(),
		"TERMBAR_STATUS_FILE="+statusFile,
		"TERMBAR_ENABLED=1",
	)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0, // fd 0 of child = slave PTY
	}

	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "termbar: start shell:", err)
		os.Exit(1)
	}
	slave.Close() // parent only needs the master side

	oldState, err := makeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Fprintln(os.Stderr, "termbar: raw mode:", err)
		os.Exit(1)
	}

	// Reserve the bottom row via DECSTBM scroll region.
	mu.Lock()
	setScrollRegion(currentRows)
	drawStatus(currentRows, "0s 000ms")
	mu.Unlock()

	// Forward SIGWINCH to child. We only update the child's reported size and
	// redraw the status bar — we do NOT re-issue DECSTBM because setting the
	// scroll region again causes kitty to clear scrollback history.
	// The child being rows-1 tall is sufficient to keep it out of our last row.
	// Forward SIGWINCH to child and re-lock the scroll region.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for range winch {
			if newWs, err := getWinsize(os.Stdin.Fd()); err == nil {
				mu.Lock()

				// 1. Update our tracker
				currentRows = int(newWs.Row)

				// 2. Tell the child process it has one fewer row
				setWinsize(master.Fd(), &Winsize{
					Row: newWs.Row - 1,
					Col: newWs.Col,
				})

				// 3. Re-assert the scroll region safely
				if !inAltScreen {
					// Set the new scroll region, then explicitly place the cursor on its
					// last row (one line above the reserved status row). We deliberately
					// don't save/restore the pre-resize cursor position here: terminals
					// often clip the cursor into the new bounds as part of their own
					// resize handling before we ever see SIGWINCH, so the "saved" position
					// can already be the bar row itself, causing a save→restore round trip
					// right back into the bar.
					fmt.Fprintf(os.Stdout, "\033[1;%dr\033[%d;1H", currentRows-1, currentRows-1)
				}

				// 4. Redraw the status bar at the new bottom row
				drawStatus(currentRows, getStatus())

				mu.Unlock()

				// 5. Notify the child shell/command to redraw its UI
				if cmd.Process != nil {
					cmd.Process.Signal(syscall.SIGWINCH)
				}
			}
		}
	}()

	// stdin → PTY master
	go io.Copy(master, os.Stdin)

	// PTY master → stdout
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				mu.Lock()
				rows := currentRows
				mu.Unlock()
				writeOutput(buf[:n], rows)
			}
			if err != nil {
				break
			}
		}
	}()

	// Status bar refresh loop
	go func() {
		for {
			time.Sleep(50 * time.Millisecond)
			mu.Lock()
			if !inAltScreen {
				if ws, err := getWinsize(os.Stdin.Fd()); err == nil {
					drawStatus(int(ws.Row), getStatus())
				}
			}
			mu.Unlock()
		}
	}()

	cmd.Wait()

	// Restore terminal, reset scroll region, clear status row.
	restoreTermios(int(os.Stdin.Fd()), oldState)
	master.Close()
	os.Remove(statusFile)
	fmt.Fprintf(os.Stdout, "\033[r\033[%d;1H\033[2K", currentRows)
}
