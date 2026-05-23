package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
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

	os.Stdout.Write(b)

	// After a reset or returning from alt-screen: re-assert scroll region
	// and redraw the status bar.
	if hasReset || altExit {
		setScrollRegion(rows)
		drawStatus(rows, getStatus())
	}
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

	// Forward SIGWINCH to child and update our scroll region.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for range winch {
			if newWs, err := getWinsize(os.Stdin.Fd()); err == nil {
				mu.Lock()
				currentRows = int(newWs.Row)
				setWinsize(master.Fd(), &Winsize{
					Row: newWs.Row - 1,
					Col: newWs.Col,
				})
				setScrollRegion(currentRows)
				mu.Unlock()
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
