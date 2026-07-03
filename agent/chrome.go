package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// Chrome is the CDP controller. It *attaches* to the supervised Chromium child
// over the DevTools Protocol (attach-not-launch, ROADMAP §9) rather than owning
// the browser lifecycle — that's the supervisor's job. Reused across web modes.
type Chrome struct {
	cfg *Config

	mu          sync.Mutex
	allocCancel context.CancelFunc
	ctxCancel   context.CancelFunc
	ctx         context.Context
	attached    bool
	dark        bool    // last applied prefers-color-scheme (for in-place theme toggles)
	zoom        float64 // last applied page zoom factor (1.0 = 100%), re-applied on navigate
	port        int     // CDP port to attach to (X kiosk vs Wayland kiosk differ)
}

// zoomAction sets the kiosk page's CSS zoom on the document root — the kiosk
// equivalent of Ctrl-+/Ctrl-− (reflows, unlike pinch scale). It must be
// re-applied after each navigation (the root style is recreated with the page).
func zoomAction(factor float64) chromedp.Action {
	z := strconv.FormatFloat(factor, 'f', -1, 64)
	expr := "document.documentElement.style.zoom='" + z + "'"
	return chromedp.ActionFunc(func(ctx context.Context) error {
		_, exc, err := runtime.Evaluate(expr).Do(ctx)
		if err != nil {
			return err
		}
		if exc != nil {
			return fmt.Errorf("zoom eval: %s", exc.Text)
		}
		return nil
	})
}

// emulateColorScheme forces the page's prefers-color-scheme to dark or light via
// CDP. We always set it explicitly (rather than only forcing dark) so toggling
// to light actually overrides a site that would otherwise follow the system —
// which on a headless kiosk is unset.
func emulateColorScheme(dark bool) chromedp.Action {
	val := "light"
	if dark {
		val = "dark"
	}
	return emulation.SetEmulatedMedia().WithFeatures([]*emulation.MediaFeature{
		{Name: "prefers-color-scheme", Value: val},
	})
}

func NewChrome(cfg *Config) *Chrome { return &Chrome{cfg: cfg, port: cfg.CDPPort, zoom: 1.0} }

// Target points the controller at a CDP port before the next Attach (the X
// kiosk uses cfg.CDPPort; the Wayland kiosk uses cfg.WaylandCDPPort). Switches
// are serialized by the supervisor, so this is only set between attachments.
func (c *Chrome) Target(port int) {
	c.mu.Lock()
	c.port = port
	c.mu.Unlock()
}

func (c *Chrome) endpoint() string {
	c.mu.Lock()
	port := c.port
	c.mu.Unlock()
	return fmt.Sprintf("http://%s:%d", c.cfg.CDPHost, port)
}

// Cold-start Chromium on a Pi 3B can take 20-30s to open the debug port;
// re-attaching to an already-running Chromium should answer in <1s, so it gets
// a short bound (a wedged-but-listening browser must not hang the switch).
const (
	attachWaitCold     = 60 * time.Second
	attachWaitReattach = 10 * time.Second
)

// connect establishes the CDP controller's connection to an already-running
// Chromium's existing page target and marks it attached. It does NOT navigate —
// Attach layers a navigation on top; Reattach deliberately does not. Any prior
// attachment is torn down first. wait bounds how long to wait for the DevTools
// page target (use attachWaitCold for a cold start).
func (c *Chrome) connect(wait time.Duration) error {
	c.Detach()

	// Wait for the DevTools endpoint to come up and expose a page target.
	targetID, err := c.waitForPageTarget(wait)
	if err != nil {
		return err
	}

	allocCtx, allocCancel := chromedp.NewRemoteAllocator(context.Background(), c.endpoint())
	ctx, ctxCancel := chromedp.NewContext(allocCtx, chromedp.WithTargetID(targetID))

	// Realize the connection with a cheap action so failures surface here. This
	// first Run allocates the browser websocket and attaches; a context deadline
	// on it would tear down the browser (per chromedp docs), so we bound it
	// out-of-band — a wedged-but-listening Chromium must not hang the switch
	// forever (POST /api/mode blocks on this).
	errc := make(chan error, 1)
	go func() { errc <- chromedp.Run(ctx) }()
	select {
	case err := <-errc:
		if err != nil {
			ctxCancel()
			allocCancel()
			return fmt.Errorf("cdp attach: %w", err)
		}
	case <-time.After(25 * time.Second):
		ctxCancel()
		allocCancel()
		return fmt.Errorf("cdp attach timed out at %s", c.endpoint())
	}

	c.mu.Lock()
	c.allocCancel = allocCancel
	c.ctxCancel = ctxCancel
	c.ctx = ctx
	c.attached = true
	c.mu.Unlock()
	return nil
}

// Attach connects to a freshly-started Chromium, binds to its existing kiosk
// tab, applies dark-mode emulation, and navigates to url. Idempotent-ish: a
// second Attach replaces any prior attachment. wait bounds how long to wait for
// the DevTools page target (use attachWaitCold for a cold start). If the initial
// navigation fails the connection is dropped, so Attached() never reports true
// for a browser we couldn't actually drive onto the page.
func (c *Chrome) Attach(url string, dark bool, wait time.Duration) error {
	if err := c.connect(wait); err != nil {
		return err
	}
	if err := c.Navigate(url, dark); err != nil {
		c.Detach()
		return err
	}
	return nil
}

// Reattach re-binds the controller to an already-running Chromium WITHOUT
// navigating — the cheap, non-disruptive recovery for a kiosk whose page is
// already correct (Chromium loads the kiosk URL itself at launch) but whose CDP
// socket was missed (the cold-start attach window closed just before the debug
// port opened) or dropped (a VT excursion). Unlike Attach it does NOT reload the
// page, so on-screen content is undisturbed; it only re-applies the persisted
// color-scheme/zoom emulations best-effort — a failure there must not drop a
// connection that is otherwise good.
func (c *Chrome) Reattach(dark bool, wait time.Duration) error {
	if err := c.connect(wait); err != nil {
		return err
	}
	c.mu.Lock()
	zoom := c.zoom
	c.mu.Unlock()
	if err := c.SetTheme(dark); err != nil {
		log.Printf("[cdp] reattach: re-apply theme failed: %v", err)
	}
	if zoom > 0 && zoom != 1.0 {
		if err := c.SetZoom(zoom); err != nil {
			log.Printf("[cdp] reattach: re-apply zoom failed: %v", err)
		}
	}
	return nil
}

// Navigate re-points the attached tab at url (and re-applies dark emulation),
// without restarting Chromium — the in-place URL switch behind POST /api/url.
func (c *Chrome) Navigate(url string, dark bool) error {
	c.mu.Lock()
	ctx := c.ctx
	attached := c.attached
	zoom := c.zoom
	c.mu.Unlock()
	if !attached || ctx == nil {
		return fmt.Errorf("cdp not attached")
	}

	// Hide scrollbars (kiosk hygiene — no visible bar; content still scrolls), force
	// the color scheme, then navigate. Both are per-target emulations re-applied each
	// navigation so they survive page changes.
	actions := []chromedp.Action{emulation.SetScrollbarsHidden(true), emulateColorScheme(dark), chromedp.Navigate(url)}
	if zoom > 0 && zoom != 1.0 {
		actions = append(actions, zoomAction(zoom)) // re-apply: navigation resets the root style
	}
	runCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if err := chromedp.Run(runCtx, actions...); err != nil {
		return fmt.Errorf("navigate %s: %w", url, err)
	}
	c.mu.Lock()
	c.dark = dark
	c.mu.Unlock()
	return nil
}

// SetZoom applies a page zoom factor to the live kiosk without navigating, and
// remembers it so later navigations keep it. 1.0 = 100%.
func (c *Chrome) SetZoom(factor float64) error {
	c.mu.Lock()
	ctx := c.ctx
	attached := c.attached
	c.mu.Unlock()
	if !attached || ctx == nil {
		return fmt.Errorf("cdp not attached")
	}
	runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := chromedp.Run(runCtx, zoomAction(factor)); err != nil {
		return fmt.Errorf("set zoom: %w", err)
	}
	c.mu.Lock()
	c.zoom = factor
	c.mu.Unlock()
	return nil
}

// eval runs a JS expression on the page, ignoring the result.
func (c *Chrome) eval(expr string) error {
	c.mu.Lock()
	ctx := c.ctx
	attached := c.attached
	c.mu.Unlock()
	if !attached || ctx == nil {
		return fmt.Errorf("cdp not attached")
	}
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return chromedp.Run(runCtx, chromedp.ActionFunc(func(c context.Context) error {
		_, exc, err := runtime.Evaluate(expr).Do(c)
		if err != nil {
			return err
		}
		if exc != nil {
			return fmt.Errorf("eval: %s", exc.Text)
		}
		return nil
	}))
}

// MsgStyle is the optional appearance of the on-screen message overlay. Zero
// values fall back to the defaults in messageCSS (top banner, 18px, light text on
// a dark bar). Position is one of top|bottom|center|top-left|top-right|
// bottom-left|bottom-right.
type MsgStyle struct {
	Size     int    `json:"size,omitempty"`     // font size in px (clamped 10–200)
	Position string `json:"position,omitempty"` // anchor (see above)
	Color    string `json:"color,omitempty"`    // text color (CSS color token)
	Bg       string `json:"bg,omitempty"`       // background color (CSS color token)
}

// cssColorRe restricts a caller-supplied color to safe CSS color tokens (hex,
// rgb()/rgba(), hsl()/hsla(), or a named color) so it can't break out of the
// inline style we build. Anything else → the default is used.
var cssColorRe = regexp.MustCompile(`^#[0-9a-fA-F]{3,8}$|^[a-zA-Z]{1,20}$|^(rgb|rgba|hsl|hsla)\([0-9.,%\s/]{1,40}\)$`)

func safeColor(s, def string) string {
	s = strings.TrimSpace(s)
	if s == "" || !cssColorRe.MatchString(s) {
		return def
	}
	return s
}

// messageCSS builds the validated inline style string for the overlay from st.
// All values are sanitized here (numeric size, allow-listed position, color
// tokens) so the result is safe to assign to element.style.cssText.
func messageCSS(st MsgStyle) string {
	size := st.Size
	if size <= 0 {
		size = 18
	} else if size < 10 {
		size = 10
	} else if size > 200 {
		size = 200
	}
	color := safeColor(st.Color, "#e6e9ef")
	bg := safeColor(st.Bg, "#151922")
	// Per-position anchor + shape. top/bottom are full-width banners; the rest are
	// floating cards. center is a centered card.
	var anchor string
	switch strings.ToLower(strings.TrimSpace(st.Position)) {
	case "", "top":
		anchor = "top:0;left:0;right:0;border-bottom:2px solid #4f9cff;"
	case "bottom":
		anchor = "bottom:0;left:0;right:0;border-top:2px solid #4f9cff;"
	case "center":
		anchor = "top:50%;left:50%;transform:translate(-50%,-50%);max-width:80vw;border-radius:12px;"
	case "top-left":
		anchor = "top:18px;left:18px;max-width:60vw;border-radius:12px;"
	case "top-right":
		anchor = "top:18px;right:18px;max-width:60vw;border-radius:12px;"
	case "bottom-left":
		anchor = "bottom:18px;left:18px;max-width:60vw;border-radius:12px;"
	case "bottom-right":
		anchor = "bottom:18px;right:18px;max-width:60vw;border-radius:12px;"
	default:
		anchor = "top:0;left:0;right:0;border-bottom:2px solid #4f9cff;"
	}
	return fmt.Sprintf("position:fixed;z-index:2147483647;%spadding:12px 18px;text-align:center;"+
		"font:600 %dpx/1.4 system-ui,sans-serif;color:%s;background:%s;box-shadow:0 2px 12px rgba(0,0,0,.4)",
		anchor, size, color, bg)
}

// ShowMessage overlays a fixed banner with text on the kiosk page (a maintenance
// notice etc.) via injected DOM; ms>0 auto-removes it. It is cleared by the next
// navigation (it lives in the page DOM). st controls appearance (size, position,
// colors) — zero values use the defaults.
func (c *Chrome) ShowMessage(text string, ms int, st MsgStyle) error {
	tjson, _ := json.Marshal(text)
	cssjson, _ := json.Marshal(messageCSS(st))
	expr := fmt.Sprintf(`(function(){var id='__sideshow_msg';var e=document.getElementById(id);`+
		`if(!e){e=document.createElement('div');e.id=id;(document.body||document.documentElement).appendChild(e);}`+
		`e.textContent=%s;e.style.cssText=%s;`+
		`if(window.__dmsgT)clearTimeout(window.__dmsgT);`+
		`if(%d>0){window.__dmsgT=setTimeout(function(){var x=document.getElementById(id);if(x)x.remove();},%d);}})()`,
		string(tjson), string(cssjson), ms, ms)
	return c.eval(expr)
}

// ClearMessage removes the overlay banner if present.
func (c *Chrome) ClearMessage() error {
	return c.eval(`(function(){var x=document.getElementById('__sideshow_msg');if(x)x.remove();})()`)
}

// ScrollPage advances the document one viewport down, wrapping back to the top
// at the bottom — the document feature's auto-advance.
func (c *Chrome) ScrollPage() error {
	const expr = `(function(){var e=document.scrollingElement||document.documentElement;` +
		`var h=e.clientHeight||window.innerHeight;` +
		`if(e.scrollTop+h>=e.scrollHeight-2){e.scrollTop=0;}else{e.scrollBy(0,h);}})()`
	return c.eval(expr)
}

// SetZoomDefault records a zoom factor to apply on the next navigation, without
// touching CDP — used at boot before Chromium is attached so the persisted zoom
// takes effect on the first page load.
func (c *Chrome) SetZoomDefault(factor float64) {
	if factor <= 0 {
		factor = 1.0
	}
	c.mu.Lock()
	c.zoom = factor
	c.mu.Unlock()
}

// SetTheme re-applies prefers-color-scheme to the live page without navigating
// or restarting Chromium — the in-place light/dark toggle behind POST /api/theme.
func (c *Chrome) SetTheme(dark bool) error {
	c.mu.Lock()
	ctx := c.ctx
	attached := c.attached
	c.mu.Unlock()
	if !attached || ctx == nil {
		return fmt.Errorf("cdp not attached")
	}
	runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := chromedp.Run(runCtx, emulateColorScheme(dark)); err != nil {
		return fmt.Errorf("set theme: %w", err)
	}
	c.mu.Lock()
	c.dark = dark
	c.mu.Unlock()
	return nil
}

// Screenshot captures the current viewport as PNG via CDP (the decided
// thumbnail source, ROADMAP §4).
func (c *Chrome) Screenshot() ([]byte, error) {
	c.mu.Lock()
	ctx := c.ctx
	attached := c.attached
	c.mu.Unlock()
	if !attached || ctx == nil {
		return nil, fmt.Errorf("cdp not attached")
	}
	var buf []byte
	runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := chromedp.Run(runCtx, chromedp.CaptureScreenshot(&buf)); err != nil {
		return nil, fmt.Errorf("captureScreenshot: %w", err)
	}
	return buf, nil
}

// Attached reports whether the controller currently holds a CDP connection.
func (c *Chrome) Attached() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attached
}

// Detach tears down the CDP connection (the browser keeps running).
func (c *Chrome) Detach() {
	c.mu.Lock()
	ctxCancel, allocCancel := c.ctxCancel, c.allocCancel
	c.ctxCancel, c.allocCancel, c.ctx, c.attached = nil, nil, nil, false
	c.mu.Unlock()
	if ctxCancel != nil {
		ctxCancel()
	}
	if allocCancel != nil {
		allocCancel()
	}
}

// waitForPageTarget polls the DevTools /json endpoint until a "page" target
// exists, returning its id. Chromium needs a beat after launch to open it.
func (c *Chrome) waitForPageTarget(timeout time.Duration) (target.ID, error) {
	deadline := time.Now().Add(timeout)
	// Generous per-poll timeout: under a cold-start load spike (notably the
	// Wayland labwc+pixman path on a Pi 3B) Chromium's /json can take several
	// seconds to answer; a 2s timeout would fail every poll for the whole window.
	client := &http.Client{Timeout: 6 * time.Second}
	url := c.endpoint() + "/json"
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err != nil {
			lastErr = err
			time.Sleep(300 * time.Millisecond)
			continue
		}
		var targets []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
			URL  string `json:"url"`
		}
		err = json.NewDecoder(resp.Body).Decode(&targets)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			time.Sleep(300 * time.Millisecond)
			continue
		}
		for _, t := range targets {
			// Skip devtools:// internal pages; we want the kiosk content tab.
			if t.Type == "page" && !strings.HasPrefix(t.URL, "devtools://") {
				return target.ID(t.ID), nil
			}
		}
		lastErr = fmt.Errorf("no page target yet")
		time.Sleep(300 * time.Millisecond)
	}
	return "", fmt.Errorf("waiting for CDP page target at %s: %w", url, lastErr)
}
