package daemon

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	urlpkg "net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lesomnus/cld/internal/devc"
	"github.com/lesomnus/cld/internal/termx"
	"golang.org/x/term"
)

// The attach stream, after the HTTP upgrade, frames only the client->daemon
// direction so a window resize can travel alongside keystrokes on the single
// multiplexed connection; the daemon->client direction is the raw pty output.
const (
	attachData   byte = 'd' // uint32 length, then that many stdin bytes
	attachResize byte = 'w' // uint16 cols, uint16 rows
)

// maxAttachData caps one data frame so a corrupt length cannot trigger a huge
// read; keystrokes and paste chunks are far smaller.
const maxAttachData = 1 << 20

// handle_attach serves GET /session/attach. It hijacks the connection and
// streams a `tmux attach` — run inside the daemon's own container — to it, so
// a client reaching this endpoint through the in-container relay attaches with
// no docker or tmux on its side. It is only offered when the daemon runs in a
// container (self_ctr set); the host `cld it` (docker exec / local tmux) covers
// the other deployment.
func (d *Daemon) handle_attach(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if d.self_ctr == "" {
		http.Error(w, "api attach requires a containerized daemon", http.StatusNotImplemented)
		return
	}
	e := d.by_name(name)
	if e == nil {
		http.Error(w, "no such devcontainer", http.StatusNotFound)
		return
	}
	cols := atoi_default(r.URL.Query().Get("cols"), 80)
	rows := atoi_default(r.URL.Query().Get("rows"), 24)
	term_name := r.URL.Query().Get("term")
	if term_name == "" {
		term_name = "xterm-256color"
	}
	session := devc.SessionName(e.snapshot().Name)

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "attach unsupported", http.StatusInternalServerError)
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: cld-attach\r\n\r\n")

	// Tie the exec's lifetime to this connection: when the client goes away the
	// frame reader ends and cancels, so termx.Stream closes the exec instead of
	// lingering until tmux next emits output.
	ctx, cancel := context.WithCancel(d.base_ctx)
	defer cancel()

	// Demux the client's frames: data -> the exec's stdin, resize -> the size
	// channel that termx.Stream applies to the tmux client's pty. pr is closed
	// on return so a frame reader blocked writing to pw is released too.
	pr, pw := io.Pipe()
	defer pr.Close()
	resize := make(chan termx.Size, 8)
	go func() {
		pw.CloseWithError(read_attach_frames(conn, pw, resize))
		cancel()
	}()

	o := termx.ExecOptions{
		Container: d.self_ctr,
		User:      strconv.Itoa(os.Getuid()),
		Env:       []string{"TERM=" + term_name, "LC_ALL=C.UTF-8"},
		Cmd:       []string{"tmux", "-S", d.cfg.TmuxSocketPath(), "attach-session", "-t", "=" + session},
	}
	size := termx.Size{Cols: uint16(cols), Rows: uint16(rows)}
	if _, err := termx.Stream(ctx, d.cli, o, pr, conn, size, resize); err != nil {
		d.log.Warn("attach ended", slog.String("name", e.snapshot().Name), slog.String("error", err.Error()))
	}
}

// read_attach_frames decodes the client->daemon direction: data frames are
// written to data (the exec stdin), resize frames are delivered to resize
// (dropped if the consumer is busy — the next resize supersedes it anyway).
func read_attach_frames(conn io.Reader, data io.Writer, resize chan<- termx.Size) error {
	br := bufio.NewReaderSize(conn, 64*1024)
	var hdr [4]byte
	for {
		t, err := br.ReadByte()
		if err != nil {
			return err
		}
		switch t {
		case attachData:
			if _, err := io.ReadFull(br, hdr[:4]); err != nil {
				return err
			}
			n := binary.BigEndian.Uint32(hdr[:4])
			if n > maxAttachData {
				return fmt.Errorf("attach data frame too large: %d", n)
			}
			if _, err := io.CopyN(data, br, int64(n)); err != nil {
				return err
			}
		case attachResize:
			if _, err := io.ReadFull(br, hdr[:4]); err != nil {
				return err
			}
			select {
			case resize <- termx.Size{Cols: binary.BigEndian.Uint16(hdr[:2]), Rows: binary.BigEndian.Uint16(hdr[2:4])}:
			default:
			}
		default:
			return fmt.Errorf("bad attach frame type %q", t)
		}
	}
}

// AttachSession attaches this terminal to a devcontainer's claude session over
// the daemon's control socket. Because it needs neither docker nor tmux on this
// side, it works when the socket is the in-container relay. It blocks until the
// user detaches or the session ends. It is the terminal call of `cld it`, so its
// stdin/winch goroutines are deliberately left to be reaped at process exit
// (os.Stdin.Read is not portably interruptible).
func AttachSession(ctx context.Context, socket string, name string) error {
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return fmt.Errorf("is `cld serve` running? %w", err)
	}
	defer conn.Close()

	// Bound the handshake so a stalled daemon/relay cannot hang `cld it`
	// forever; cleared once the 101 and headers are consumed.
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	fd := int(os.Stdin.Fd())
	cols, rows := 80, 24
	if w, h, err := term.GetSize(fd); err == nil && w > 0 && h > 0 {
		cols, rows = w, h
	}
	term_name := os.Getenv("TERM")
	if term_name == "" {
		term_name = "xterm-256color"
	}

	req := fmt.Sprintf("GET /session/attach?name=%s&cols=%d&rows=%d&term=%s HTTP/1.1\r\n"+
		"Host: cld\r\nConnection: Upgrade\r\nUpgrade: cld-attach\r\n\r\n",
		urlpkg.QueryEscape(name), cols, rows, urlpkg.QueryEscape(term_name))
	if _, err := io.WriteString(conn, req); err != nil {
		return err
	}

	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		return err
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	if !strings.Contains(status, " 101 ") {
		return fmt.Errorf("attach failed: %s", strings.TrimSpace(status))
	}
	conn.SetReadDeadline(time.Time{}) // handshake done; the attach is long-lived

	if term.IsTerminal(fd) {
		if old, err := term.MakeRaw(fd); err == nil {
			defer term.Restore(fd, old)
		}
	}

	var wmu sync.Mutex
	send_resize := func() {
		w, h, err := term.GetSize(fd)
		if err != nil || w <= 0 || h <= 0 {
			return
		}
		var b [5]byte
		b[0] = attachResize
		binary.BigEndian.PutUint16(b[1:3], uint16(w))
		binary.BigEndian.PutUint16(b[3:5], uint16(h))
		wmu.Lock()
		conn.Write(b[:])
		wmu.Unlock()
	}

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			send_resize()
		}
	}()

	go func() {
		buf := make([]byte, 32*1024)
		var hdr [5]byte
		hdr[0] = attachData
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				binary.BigEndian.PutUint32(hdr[1:5], uint32(n))
				wmu.Lock()
				conn.Write(hdr[:])
				conn.Write(buf[:n])
				wmu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	// Any pty bytes that arrived with the 101 response are buffered in br.
	_, err = io.Copy(os.Stdout, br)
	return err
}

func atoi_default(s string, def int) int {
	if v, err := strconv.Atoi(s); err == nil && v > 0 {
		return v
	}
	return def
}
