package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Plymouth manages the boot splash: whether it shows ("splash" in the kernel
// cmdline), which message it renders, and the splash image. The agent owns the
// sideshow Plymouth theme — a script theme that paints a black background, a
// centered image, and a status line — and regenerates the theme script (with the
// message inlined) on change, then rebuilds the initramfs so the new splash is
// embedded for the *next* boot. All of it is best-effort and node-side: on a host
// without Plymouth (a dev box) Info() just reports installed=false.
type Plymouth struct {
	cfg *Config
	mu  sync.Mutex // serializes initramfs rebuilds (slow) + cmdline edits
}

func NewPlymouth(cfg *Config) *Plymouth { return &Plymouth{cfg: cfg} }

// PlymouthInfo is the boot-splash state served by GET /api/plymouth.
type PlymouthInfo struct {
	Installed bool   `json:"installed"`         // the plymouth binary is present
	Enabled   bool   `json:"enabled"`           // "splash" is in the kernel cmdline (shows on next boot)
	Theme     string `json:"theme"`             // the configured theme name
	ThemeSet  bool   `json:"theme_set"`         // the sideshow theme dir exists on the node
	Message   string `json:"message,omitempty"` // the status line the splash renders
	ImageSet  bool   `json:"image_set"`         // a splash image is present
	Note      string `json:"note,omitempty"`    // a hint (e.g. "reboot to apply")
}

func (p *Plymouth) themeDir() string  { return p.cfg.PlymouthThemeDir }
func (p *Plymouth) imagePath() string { return filepath.Join(p.themeDir(), "splash.png") }
func (p *Plymouth) msgPath() string   { return filepath.Join(p.themeDir(), "message.txt") }
func (p *Plymouth) scriptPath() string {
	return filepath.Join(p.themeDir(), p.cfg.PlymouthTheme+".script")
}

// plymouthInstalled reports whether plymouth is on the node (the binary exists).
func plymouthInstalled() bool {
	_, err := exec.LookPath("plymouth")
	return err == nil
}

// Info reports the current boot-splash state (no mutation).
func (p *Plymouth) Info() PlymouthInfo {
	info := PlymouthInfo{
		Installed: plymouthInstalled(),
		Theme:     p.cfg.PlymouthTheme,
		Enabled:   p.cmdlineHasSplash(),
		ThemeSet:  fileExists(filepath.Join(p.themeDir(), p.cfg.PlymouthTheme+".plymouth")),
		Message:   p.currentMessage(),
		ImageSet:  fileExists(p.imagePath()),
	}
	return info
}

// currentMessage returns the persisted splash message, or a default.
func (p *Plymouth) currentMessage() string {
	if b, err := os.ReadFile(p.msgPath()); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	return "Loading, please wait…"
}

// cmdlineHasSplash reports whether the kernel cmdline contains the "splash"
// token (the switch that makes Plymouth draw the graphical splash).
func (p *Plymouth) cmdlineHasSplash() bool {
	b, err := os.ReadFile(p.cfg.PlymouthCmdline)
	if err != nil {
		return false
	}
	for _, tok := range strings.Fields(string(b)) {
		if tok == "splash" {
			return true
		}
	}
	return false
}

// SetEnabled adds or removes "splash" (and "quiet") in the kernel cmdline so the
// graphical splash shows — or doesn't — on the next boot. It does NOT reboot;
// the change applies at the next boot. 503 when there is no cmdline file (a dev
// host / unexpected layout).
func (p *Plymouth) SetEnabled(enabled bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	path := p.cfg.PlymouthCmdline
	b, err := os.ReadFile(path)
	if err != nil {
		return &apiError{code: 503, err: fmt.Errorf("kernel cmdline %s not found (boot-splash toggle is node-only): %w", path, err)}
	}
	// cmdline.txt is a single line of space-separated tokens.
	line := strings.TrimRight(string(b), "\n")
	fields := strings.Fields(line)
	has := map[string]bool{}
	out := make([]string, 0, len(fields)+2)
	for _, f := range fields {
		if f == "splash" || f == "quiet" {
			has[f] = true
			if !enabled {
				continue // drop both when disabling
			}
		}
		out = append(out, f)
	}
	if enabled {
		if !has["quiet"] {
			out = append(out, "quiet")
		}
		if !has["splash"] {
			out = append(out, "splash")
		}
	}
	newLine := strings.Join(out, " ") + "\n"
	if newLine == string(b) {
		return nil // no change
	}
	if err := os.WriteFile(path, []byte(newLine), 0o644); err != nil {
		return &apiError{code: 500, err: fmt.Errorf("write %s: %w", path, err)}
	}
	return nil
}

// SetMessage stores a new splash status line, regenerates the theme script with
// it inlined, and rebuilds the initramfs so it takes effect next boot.
func (p *Plymouth) SetMessage(msg string) error {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		msg = "Loading, please wait…"
	}
	if len(msg) > 200 {
		return &apiError{code: 400, err: fmt.Errorf("message too long (max 200 chars)")}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !fileExists(p.themeDir()) {
		return &apiError{code: 503, err: fmt.Errorf("theme dir %s missing — run the deploy 'install' step on the node first", p.themeDir())}
	}
	if err := os.WriteFile(p.msgPath(), []byte(msg+"\n"), 0o644); err != nil {
		return &apiError{code: 500, err: fmt.Errorf("write message: %w", err)}
	}
	if err := os.WriteFile(p.scriptPath(), []byte(plymouthScript(msg)), 0o644); err != nil {
		return &apiError{code: 500, err: fmt.Errorf("write theme script: %w", err)}
	}
	return p.rebuildInitramfs()
}

// SetImage validates a PNG, writes it as the splash image, and rebuilds the
// initramfs so it is embedded for the next boot.
func (p *Plymouth) SetImage(png []byte) error {
	if !isPNG(png) {
		return &apiError{code: 400, err: fmt.Errorf("image must be a PNG")}
	}
	if len(png) > 8<<20 {
		return &apiError{code: 400, err: fmt.Errorf("image too large (max 8 MiB)")}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !fileExists(p.themeDir()) {
		return &apiError{code: 503, err: fmt.Errorf("theme dir %s missing — run the deploy 'install' step on the node first", p.themeDir())}
	}
	if err := os.WriteFile(p.imagePath(), png, 0o644); err != nil {
		return &apiError{code: 500, err: fmt.Errorf("write image: %w", err)}
	}
	return p.rebuildInitramfs()
}

// rebuildInitramfs re-embeds the theme into the initrd so splash changes show at
// the next boot. Plymouth reads its theme from the initramfs at early boot, so a
// theme edit without this would not appear. Slow (tens of seconds on a Pi); the
// caller holds p.mu. A missing update-initramfs (non-initramfs node) is tolerated.
func (p *Plymouth) rebuildInitramfs() error {
	if _, err := exec.LookPath("update-initramfs"); err != nil {
		return nil // no initramfs tooling — nothing to rebuild
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "update-initramfs", "-u").CombinedOutput(); err != nil {
		return &apiError{code: 500, err: fmt.Errorf("update-initramfs: %w: %s", err, tailString(string(out), 300))}
	}
	return nil
}

// isPNG reports whether b starts with the 8-byte PNG signature.
func isPNG(b []byte) bool {
	sig := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	if len(b) < len(sig) {
		return false
	}
	for i, c := range sig {
		if b[i] != c {
			return false
		}
	}
	return true
}

// plymouthScript renders the sideshow theme's script (Plymouth's script
// language) with msg inlined as the status line. Black background, a centered
// splash.png, and the message below it. msg is escaped for a Plymouth string
// literal (it sits inside double quotes).
func plymouthScript(msg string) string {
	esc := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", " ").Replace(msg)
	return `# sideshow boot splash — generated by the agent (do not hand-edit; use /api/plymouth).
Window.SetBackgroundTopColor(0.0, 0.0, 0.0);
Window.SetBackgroundBottomColor(0.0, 0.0, 0.0);

screen_width = Window.GetWidth();
screen_height = Window.GetHeight();

# Centered splash image (optional — guarded so a missing file can't crash boot).
splash.image = Image("splash.png");
if (splash.image) {
  splash.sprite = Sprite(splash.image);
  splash.sprite.SetX(Window.GetX() + screen_width / 2 - splash.image.GetWidth() / 2);
  splash.sprite.SetY(Window.GetY() + screen_height / 2 - splash.image.GetHeight() / 2);
}

# Status line below the image.
message = "` + esc + `";
msg.image = Image.Text(message, 0.9, 0.9, 0.9);
msg.sprite = Sprite(msg.image);
msg.sprite.SetX(Window.GetX() + screen_width / 2 - msg.image.GetWidth() / 2);
msg.sprite.SetY(Window.GetY() + screen_height * 0.72);

# Keep the boot/refresh callbacks defined (Plymouth expects them); the splash is
# static so they do nothing.
fun refresh_callback() { }
Plymouth.SetRefreshFunction(refresh_callback);
`
}
