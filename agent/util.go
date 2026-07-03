package main

import (
	"bytes"
	"embed"
	"image"
	"image/png"
	"io"
	"sync"
)

//go:embed web/index.html
var indexHTML []byte

// showHTML is the self-contained mixed-media playlist player, served at /show.
// The kiosk is pointed at it; it fetches /api/playlist-media and advances through
// images/videos/audio/docs client-side (interval, or a media element's end).
//
//go:embed web/show.html
var showHTML []byte

// setupHTML is the first-run setup wizard, served at /setup (auth-exempt only
// while the node is not yet SetupComplete). It detects the node via /api/setup,
// installs feature prerequisites, and finishes → SetSetupComplete(true).
//
//go:embed web/setup.html
var setupHTML []byte

// novncFS is the vendored noVNC viewer (core/ + vendor/ + index.html), served at
// /vnc so the live-screen view is fully self-contained in the binary — no
// internet, no websockify. See web/novnc/VENDORED.md.
//
//go:embed web/novnc
var novncFS embed.FS

// prefixMu serializes ALL prefixed writes (every child's stdout+stderr share it)
// so line output to the shared os.Stdout/os.Stderr doesn't interleave mid-line.
var prefixMu sync.Mutex

// linePrefixer tags each complete line from a child with its mode label and
// mirrors it into the /api/logs ring. It splits on '\n' inside Write — no
// background goroutine, so there is nothing to leak when the child exits (the
// old io.Pipe + scanner goroutine never got EOF, because os/exec doesn't close
// the io.Writer it was handed, leaking 2 goroutines per child exit).
type linePrefixer struct {
	w      io.Writer
	prefix string
	buf    []byte // partial trailing line not yet terminated by '\n'
}

func (p *linePrefixer) Write(b []byte) (int, error) {
	prefixMu.Lock()
	defer prefixMu.Unlock()
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		line := string(p.buf[:i])
		p.buf = p.buf[i+1:]
		p.w.Write([]byte(p.prefix + line + "\n"))
		logs.add(p.prefix + line)
	}
	return len(b), nil
}

// prefixWriter wraps an io.Writer so each line from a child process is tagged
// with its mode label in the agent's logs/journal and the /api/logs ring.
func prefixWriter(w io.Writer, label string) io.Writer {
	return &linePrefixer{w: w, prefix: "[" + label + "] "}
}

// downscalePNG shrinks a PNG so its width is at most maxW, preserving aspect
// ratio (nearest-neighbor — cheap, no extra deps; thumbnails don't need more).
// Returns the original bytes if it's already small enough or on any error.
func downscalePNG(in []byte, maxW int) ([]byte, error) {
	src, err := png.Decode(bytes.NewReader(in))
	if err != nil {
		return in, err
	}
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= maxW || maxW <= 0 {
		return in, nil
	}
	dw := maxW
	dh := sh * maxW / sw
	if dh < 1 {
		dh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		sy := b.Min.Y + y*sh/dh
		for x := 0; x < dw; x++ {
			sx := b.Min.X + x*sw/dw
			dst.Set(x, y, src.At(sx, sy))
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, dst); err != nil {
		return in, err
	}
	return out.Bytes(), nil
}
