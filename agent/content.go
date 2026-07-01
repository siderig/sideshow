package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Content drives signage behaviour over the web kiosk: a URL playlist that
// rotates on a timer, a periodic page reload (kiosk hygiene), an injected image
// slideshow, and a document (PDF/slides) viewer with auto-scroll. It navigates /
// injects in place via the supervisor (CDP, no Chromium restart) and only acts
// while a web mode is actually on screen — so a manual switch to console/off/
// standby pauses it. It also holds the per-output content model (scaffold: only
// the primary output renders today; secondary content is persisted + reported).
// Persisted next to the display state in content.json.
//
// Exactly one "page owner" drives the single kiosk at a time. tick() arbitrates
// in strict priority: slideshow > document > playlist/reload. Enabling any one of
// slideshow/document/playlist disables the others so they can't fight.
type Content struct {
	sup  *Supervisor
	path string

	mu          sync.Mutex
	playlist    []string
	intervalSec int
	enabled     bool
	reloadMin   int
	idx         int
	lastAdvance time.Time
	lastReload  time.Time

	// slideshow (injected over the web kiosk via CDP)
	slideImages   []string
	slideInterval int // seconds
	slideFit      string
	slideTrans    string
	slideEnabled  bool
	lastSlideKick time.Time

	// document (PDF/slides over the web kiosk; auto-scroll)
	docSrc      string // an http(s) URL or the resolved /docfs/<rel> served URL
	docAdvanceS int
	docEnabled  bool
	lastDocAdv  time.Time
	docsDir     string

	// per-output content (scaffold for secondary outputs; primary renders)
	outputContent map[string]OutputContent

	// cross-link to the display manager (set post-construction in main.go) so
	// SetOutputContent can resolve the primary output name and route content.
	display *Display

	// localBase is the agent's own loopback base URL (e.g. http://127.0.0.1:80),
	// used to turn a /docfs/<rel> document path into an absolute URL the kiosk can
	// actually fetch — a root-relative path would resolve against the kiosk's
	// current (external) origin, not this agent.
	localBase string
}

// localAgentBase derives the agent's own loopback base URL from the listen
// address, so the locally-running kiosk can fetch /docfs and /slideshow over
// 127.0.0.1 regardless of what external origin it is currently showing.
func localAgentBase(addr string) string {
	port := "80"
	if i := strings.LastIndex(addr, ":"); i >= 0 && i+1 < len(addr) {
		port = addr[i+1:]
	}
	return "http://127.0.0.1:" + port
}

// ContentInfo is the JSON for GET /api/content / the webUI.
type ContentInfo struct {
	Playlist  []string `json:"playlist"`
	IntervalS int      `json:"interval_s"`
	Enabled   bool     `json:"enabled"`
	ReloadMin int      `json:"reload_min"`
	Index     int      `json:"index"`
}

// SlideshowInfo is the JSON for GET/POST /api/slideshow.
type SlideshowInfo struct {
	Images     []string `json:"images"`
	IntervalS  int      `json:"interval_s"`
	Fit        string   `json:"fit"`
	Transition string   `json:"transition"`
	Enabled    bool     `json:"enabled"`
}

// DocumentInfo is the JSON for GET/POST /api/document.
type DocumentInfo struct {
	Src          string `json:"src"`
	AutoAdvanceS int    `json:"auto_advance_s"`
	Enabled      bool   `json:"enabled"`
}

// OutputContent is what an output should show. type ∈ web|media|slideshow|off|
// mirror. Only the primary output's content renders today; secondary content is
// persisted + reported (rendering deferred — see node-api.md).
type OutputContent struct {
	Type string `json:"type"`
	URL  string `json:"url,omitempty"`
	Path string `json:"path,omitempty"`
}

type persistedSlideshow struct {
	Images     []string `json:"images"`
	IntervalS  int      `json:"interval_s"`
	Fit        string   `json:"fit"`
	Transition string   `json:"transition"`
	Enabled    bool     `json:"enabled"`
}

type persistedDocument struct {
	Src          string `json:"src"`
	AutoAdvanceS int    `json:"auto_advance_s"`
	Enabled      bool   `json:"enabled"`
}

type persistedContent struct {
	Playlist      []string                 `json:"playlist"`
	IntervalS     int                      `json:"interval_s"`
	Enabled       bool                     `json:"enabled"`
	ReloadMin     int                      `json:"reload_min"`
	Slideshow     *persistedSlideshow      `json:"slideshow,omitempty"`
	Document      persistedDocument        `json:"document"`
	OutputContent map[string]OutputContent `json:"output_content,omitempty"`
}

const defaultSlideInterval = 6

// maxSlideshowImages bounds how many images one slideshow may hold (the page
// preloads them and the agent re-injects the list periodically).
const maxSlideshowImages = 200

func NewContent(cfg *Config, sup *Supervisor) *Content {
	c := &Content{
		sup:           sup,
		intervalSec:   30,
		slideInterval: defaultSlideInterval,
		slideFit:      "contain",
		slideTrans:    "fade",
		docsDir:       cfg.DocsDir,
		outputContent: map[string]OutputContent{},
		localBase:     localAgentBase(cfg.Addr),
	}
	if cfg.StateFile != "" {
		c.path = filepath.Join(filepath.Dir(cfg.StateFile), "content.json")
	}
	c.load()
	return c
}

// SetDisplay wires the display manager back-link (post-construction to avoid a
// constructor cycle). Used by SetOutputContent to resolve/route the primary.
func (c *Content) SetDisplay(d *Display) { c.display = d }

func (c *Content) load() {
	if c.path == "" {
		return
	}
	b, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	var p persistedContent
	if json.Unmarshal(b, &p) != nil {
		return
	}
	c.playlist = filterHTTP(p.Playlist)
	if p.IntervalS > 0 {
		c.intervalSec = p.IntervalS
	}
	c.enabled = p.Enabled && len(c.playlist) > 0
	if p.ReloadMin >= 0 {
		c.reloadMin = p.ReloadMin
	}
	if p.Slideshow != nil {
		c.slideImages = filterImages(p.Slideshow.Images)
		if p.Slideshow.IntervalS > 0 {
			c.slideInterval = p.Slideshow.IntervalS
		}
		c.slideFit = normFit(p.Slideshow.Fit)
		c.slideTrans = normTransition(p.Slideshow.Transition)
		c.slideEnabled = p.Slideshow.Enabled && len(c.slideImages) > 0
	}
	c.docSrc = p.Document.Src
	if p.Document.AutoAdvanceS > 0 {
		c.docAdvanceS = p.Document.AutoAdvanceS
	}
	c.docEnabled = p.Document.Enabled && c.docSrc != ""
	if p.OutputContent != nil {
		c.outputContent = p.OutputContent
	}
}

func (c *Content) save() {
	if c.path == "" {
		return
	}
	c.mu.Lock()
	p := persistedContent{
		Playlist:  c.playlist,
		IntervalS: c.intervalSec,
		Enabled:   c.enabled,
		ReloadMin: c.reloadMin,
		Slideshow: &persistedSlideshow{
			Images:     c.slideImages,
			IntervalS:  c.slideInterval,
			Fit:        c.slideFit,
			Transition: c.slideTrans,
			Enabled:    c.slideEnabled,
		},
		Document: persistedDocument{
			Src:          c.docSrc,
			AutoAdvanceS: c.docAdvanceS,
			Enabled:      c.docEnabled,
		},
		OutputContent: cloneOutputContent(c.outputContent),
	}
	c.mu.Unlock()
	b, _ := json.MarshalIndent(p, "", "  ")
	_ = os.MkdirAll(filepath.Dir(c.path), 0o755)
	if err := os.WriteFile(c.path, b, 0o644); err != nil {
		log.Printf("[content] save: %v", err)
	}
}

func (c *Content) Start() {
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for range t.C {
			c.tick()
		}
	}()
}

func (c *Content) tick() {
	st := c.sup.Status()
	if st.Type != ModeWeb || st.State != stateRunning {
		return // only while the web kiosk is on screen
	}
	now := time.Now()

	c.mu.Lock()
	// Strict priority: slideshow > document > playlist/reload. Exactly one owns
	// the page (the setters keep the others disabled).
	slideOn := c.slideEnabled && len(c.slideImages) > 0
	slideKickDue := slideOn && now.Sub(c.lastSlideKick) >= 5*time.Second
	// A document owns the page whenever it is enabled — even with auto-advance off
	// (a static PDF). Otherwise a configured periodic reload would keep reloading
	// it out from under the viewer.
	docOn := c.docEnabled && c.docSrc != ""
	docAdvDue := docOn && c.docAdvanceS > 0 && now.Sub(c.lastDocAdv) >= time.Duration(c.docAdvanceS)*time.Second
	enabled, n, interval := c.enabled, len(c.playlist), c.intervalSec
	advanceDue := enabled && n > 1 && interval > 0 && now.Sub(c.lastAdvance) >= time.Duration(interval)*time.Second
	reloadDue := c.reloadMin > 0 && now.Sub(c.lastReload) >= time.Duration(c.reloadMin)*time.Minute
	images := append([]string(nil), c.slideImages...)
	slideMs := c.slideInterval * 1000
	fit, tr := c.slideFit, c.slideTrans
	c.mu.Unlock()

	if slideOn {
		if slideKickDue {
			c.kickSlideshow(images, slideMs, fit, tr, now)
		}
		return // slideshow owns the page
	}
	if docOn {
		if docAdvDue && c.sup.AdvanceDocument() {
			c.mu.Lock()
			c.lastDocAdv = now
			c.mu.Unlock()
		}
		return // document owns the page
	}
	if advanceDue {
		c.advance() // advancing loads a page; don't also reload this tick
		return
	}
	if reloadDue {
		c.mu.Lock()
		c.lastReload = now
		c.mu.Unlock()
		if err := c.sup.ReloadWeb(); err != nil {
			log.Printf("[content] periodic reload: %v", err)
		}
	}
}

// kickSlideshow re-asserts the on-page slideshow overlay (idempotent on the page
// side). The timer is committed only on success so a detached tick retries.
func (c *Content) kickSlideshow(images []string, intervalMs int, fit, tr string, now time.Time) {
	if c.sup.RunSlideshowIfWeb(images, intervalMs, fit, tr) {
		c.mu.Lock()
		c.lastSlideKick = now
		c.mu.Unlock()
	}
}

// advance moves to the next slide via the supervisor's atomic NavigateIfWeb
// (which re-navigates in place ONLY if a running web kiosk is still on screen).
// The index/timer are committed only on success, so a skipped tick (operator
// switched away, or CDP momentarily detached) retries the same slide rather than
// dropping it.
func (c *Content) advance() {
	c.mu.Lock()
	next := (c.idx + 1) % len(c.playlist)
	url := c.playlist[next]
	c.mu.Unlock()
	if c.sup.NavigateIfWeb(url) {
		now := time.Now()
		c.mu.Lock()
		c.idx = next
		c.lastAdvance = now
		c.lastReload = now
		c.mu.Unlock()
	}
}

// SetPlaylist replaces the playlist and starts/stops rotation. Enabling the
// playlist disables the slideshow/document so only one owns the page.
func (c *Content) SetPlaylist(urls []string, intervalSec int, enabled bool) error {
	clean := filterHTTP(urls)
	if enabled && len(clean) == 0 {
		return &apiError{code: 400, err: fmt.Errorf("playlist needs at least one http(s) URL")}
	}
	if intervalSec < 5 {
		intervalSec = 5
	}
	c.mu.Lock()
	c.playlist = clean
	c.intervalSec = intervalSec
	c.enabled = enabled && len(clean) > 0
	c.idx = 0
	now := time.Now()
	c.lastAdvance = now
	c.lastReload = now // restart the auto-reload countdown too
	if c.enabled {
		c.slideEnabled = false
		c.docEnabled = false
	}
	first := ""
	if c.enabled {
		first = clean[0]
	}
	c.mu.Unlock()
	c.save()
	if first != "" {
		c.sup.StopSlideshowIfWeb() // clear any lingering overlay
		c.sup.NavigateIfWeb(first) // jump to the first item if a web kiosk is on screen
	}
	return nil
}

// SetReload sets the periodic reload interval in minutes (0 disables).
func (c *Content) SetReload(minutes int) {
	if minutes < 0 {
		minutes = 0
	}
	c.mu.Lock()
	c.reloadMin = minutes
	c.lastReload = time.Now()
	c.mu.Unlock()
	c.save()
}

// SetSlideshow configures the image slideshow. Enabling it disables the
// playlist/document and injects the overlay; disabling removes it.
func (c *Content) SetSlideshow(images []string, intervalS int, fit, transition string, enabled bool) (SlideshowInfo, error) {
	clean := filterImages(images)
	if enabled && len(clean) == 0 {
		return SlideshowInfo{}, &apiError{code: 400, err: fmt.Errorf("slideshow needs at least one http(s):// image URL")}
	}
	// Bound the list the page preloads/cycles (and the agent re-injects every few
	// seconds) so a huge submission can't exhaust the kiosk's memory on a Pi.
	if len(clean) > maxSlideshowImages {
		log.Printf("[content] slideshow truncated %d→%d images", len(clean), maxSlideshowImages)
		clean = clean[:maxSlideshowImages]
	}
	if intervalS <= 0 {
		intervalS = defaultSlideInterval
	}
	c.mu.Lock()
	c.slideImages = clean
	c.slideInterval = intervalS
	c.slideFit = normFit(fit)
	c.slideTrans = normTransition(transition)
	c.slideEnabled = enabled && len(clean) > 0
	if c.slideEnabled {
		c.enabled = false // the playlist yields the page
		c.docEnabled = false
	}
	c.lastSlideKick = time.Time{}
	on := c.slideEnabled
	out := SlideshowInfo{Images: clean, IntervalS: intervalS, Fit: c.slideFit, Transition: c.slideTrans, Enabled: on}
	kickImgs := append([]string(nil), c.slideImages...)
	kickMs := c.slideInterval * 1000
	kFit, kTr := c.slideFit, c.slideTrans
	c.mu.Unlock()
	c.save()
	if on {
		if c.sup.RunSlideshowIfWeb(kickImgs, kickMs, kFit, kTr) {
			c.mu.Lock()
			c.lastSlideKick = time.Now()
			c.mu.Unlock()
		}
	} else {
		c.sup.StopSlideshowIfWeb()
	}
	return out, nil
}

// SetDocument shows a PDF/slides document over the web kiosk (an http(s) URL or
// a relative path under -docs-dir). Enabling disables the playlist/slideshow and
// navigates the kiosk to the viewer URL; disabling stops auto-advance (the page
// is left as-is — the operator switches the URL to leave it).
func (c *Content) SetDocument(src string, autoAdvanceS int, enabled bool) (DocumentInfo, error) {
	served, err := validateDocSrc(c.docsDir, src)
	if err != nil {
		return DocumentInfo{}, &apiError{code: 400, err: err}
	}
	if autoAdvanceS < 0 {
		autoAdvanceS = 0
	}
	c.mu.Lock()
	c.docSrc = served
	c.docAdvanceS = autoAdvanceS
	c.docEnabled = enabled && served != ""
	if c.docEnabled {
		c.enabled = false
		c.slideEnabled = false
	}
	c.lastDocAdv = time.Now()
	on := c.docEnabled
	out := DocumentInfo{Src: served, AutoAdvanceS: autoAdvanceS, Enabled: on}
	c.mu.Unlock()
	c.save()
	if on {
		c.sup.StopSlideshowIfWeb()
		c.sup.NavigateIfWeb(withPDFViewerParams(c.absDocURL(served)))
	}
	return out, nil
}

// absDocURL turns a /docfs/<rel> served path into an absolute loopback URL the
// kiosk can fetch (the kiosk is normally on an external origin, so a root-
// relative path would 404 against that site). http(s) sources pass through.
func (c *Content) absDocURL(served string) string {
	if strings.HasPrefix(served, "/docfs/") && c.localBase != "" {
		return c.localBase + served
	}
	return served
}

// SetOutputContent assigns content to an output (validated + persisted). The
// PRIMARY output's content is rendered through the running web kiosk; any other
// output's content is persisted + reported only (rendering deferred — see
// node-api.md). Returns 400 on a bad type/missing fields.
func (c *Content) SetOutputContent(name string, oc OutputContent) error {
	oc.Type = strings.ToLower(strings.TrimSpace(oc.Type))
	oc.URL = strings.TrimSpace(oc.URL)
	oc.Path = strings.TrimSpace(oc.Path)
	switch oc.Type {
	case "off", "mirror":
		oc.URL, oc.Path = "", ""
	case "web", "slideshow":
		if len(filterHTTP([]string{oc.URL})) == 0 {
			return &apiError{code: 400, err: fmt.Errorf("%s output content needs an http(s) url", oc.Type)}
		}
	case "media":
		if oc.URL == "" && oc.Path == "" {
			return &apiError{code: 400, err: fmt.Errorf("media output content needs a url or path")}
		}
	default:
		return &apiError{code: 400, err: fmt.Errorf("unknown output content type %q (web|media|slideshow|off|mirror)", oc.Type)}
	}

	primary := ""
	if c.display != nil {
		primary = c.display.PrimaryName()
	}
	if name == "" {
		name = primary
	}

	c.mu.Lock()
	if c.outputContent == nil {
		c.outputContent = map[string]OutputContent{}
	}
	c.outputContent[name] = oc
	c.mu.Unlock()
	c.save()

	if name != primary || primary == "" {
		log.Printf("[content] output %q content (%s) assigned; secondary-output rendering deferred — see node-api.md", name, oc.Type)
		return nil
	}

	// Primary output: route to the kiosk. Each branch keeps the single-page-owner
	// invariant by clearing the other content owners (or, for non-web modes,
	// switching the supervisor — which leaves web mode entirely).
	switch oc.Type {
	case "web":
		c.DisableOwners() // a plain web URL is no one's "content owner"
		m := Mode{Type: ModeWeb, Params: map[string]any{"url": oc.URL}}
		if cur := c.sup.Status(); cur.Type == ModeWeb && cur.Display != "" {
			m.Display = cur.Display // don't flip the compositor backend
		}
		return c.sup.Switch(m)
	case "slideshow":
		// A single still image on the primary via the slideshow injector.
		c.mu.Lock()
		c.slideImages = filterImages([]string{oc.URL})
		c.slideEnabled = len(c.slideImages) > 0
		c.enabled = false // hand the page to the slideshow
		c.docEnabled = false
		imgs := append([]string(nil), c.slideImages...)
		ms := c.slideInterval * 1000
		fit, tr := c.slideFit, c.slideTrans
		c.mu.Unlock()
		c.save()
		c.sup.RunSlideshowIfWeb(imgs, ms, fit, tr)
	case "media":
		c.DisableOwners() // media leaves web mode; no page owner should re-assert
		params := map[string]any{}
		if oc.URL != "" {
			params["url"] = oc.URL
		} else {
			params["path"] = oc.Path
		}
		return c.sup.Switch(Mode{Type: ModeMedia, Display: DisplayCompositor, Params: params})
	case "off":
		c.DisableOwners()
		return c.sup.Switch(Mode{Type: ModeOff})
	case "mirror":
		log.Printf("[content] output %q content (mirror) assigned; nothing to render on the primary", name)
	}
	return nil
}

// DisableOwners clears every content page-owner (playlist/slideshow/document) so
// a direct web navigation or a non-web mode switch takes the screen without a
// timer re-asserting stale content on top of it. Best-effort overlay clear.
func (c *Content) DisableOwners() {
	c.mu.Lock()
	changed := c.enabled || c.slideEnabled || c.docEnabled
	c.enabled = false
	c.slideEnabled = false
	c.docEnabled = false
	c.mu.Unlock()
	if changed {
		c.save()
		c.sup.StopSlideshowIfWeb()
	}
}

// cloneOutputContent returns a shallow copy of the per-output content map so
// save() can marshal a snapshot without holding the lock (a concurrent
// SetOutputContent write during json.Marshal would otherwise panic).
func cloneOutputContent(m map[string]OutputContent) map[string]OutputContent {
	if m == nil {
		return nil
	}
	out := make(map[string]OutputContent, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (c *Content) Info() ContentInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return ContentInfo{Playlist: c.playlist, IntervalS: c.intervalSec, Enabled: c.enabled, ReloadMin: c.reloadMin, Index: c.idx}
}

func (c *Content) SlideshowInfo() SlideshowInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	imgs := append([]string(nil), c.slideImages...)
	return SlideshowInfo{Images: imgs, IntervalS: c.slideInterval, Fit: c.slideFit, Transition: c.slideTrans, Enabled: c.slideEnabled}
}

func (c *Content) DocInfo() DocumentInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return DocumentInfo{Src: c.docSrc, AutoAdvanceS: c.docAdvanceS, Enabled: c.docEnabled}
}

// For returns the assigned content for an output (zero = {Type:"off"}).
func (c *Content) For(name string) OutputContent {
	c.mu.Lock()
	defer c.mu.Unlock()
	if oc, ok := c.outputContent[name]; ok {
		return oc
	}
	return OutputContent{Type: "off"}
}

// filterHTTP keeps only well-formed http(s) URLs, trimmed.
func filterHTTP(urls []string) []string {
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			out = append(out, u)
		}
	}
	return out
}

// filterImages keeps only http(s) image URLs. Local paths / file:// are rejected:
// the slideshow is injected over an http(s) kiosk page, and Chromium blocks an
// http(s) page from loading file:// subresources (so they would render blank),
// and accepting arbitrary absolute paths would be a local-file-read surface.
func filterImages(items []string) []string {
	return filterHTTP(items)
}

// normFit normalizes the slideshow object-fit (contain|cover), default contain.
func normFit(s string) string {
	if strings.ToLower(strings.TrimSpace(s)) == "cover" {
		return "cover"
	}
	return "contain"
}

// normTransition normalizes the slideshow transition (none|fade), default fade.
func normTransition(s string) string {
	if strings.ToLower(strings.TrimSpace(s)) == "none" {
		return "none"
	}
	return "fade"
}

// validateDocSrc resolves a document source to a URL the kiosk can load. An
// http(s) URL passes through. Anything else is treated as a relative path under
// docsDir and resolved to a /docfs/<rel> served URL, rejecting traversal,
// absolute paths, symlink escapes, non-files, and other schemes. Returns the
// served URL (or "") and a user-facing error.
func validateDocSrc(docsDir, src string) (string, error) {
	src = strings.TrimSpace(src)
	if src == "" {
		return "", nil
	}
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		return src, nil
	}
	if strings.Contains(src, "://") {
		return "", fmt.Errorf("document src must be an http(s) URL or a relative path under the docs dir")
	}
	if docsDir == "" {
		return "", fmt.Errorf("no -docs-dir configured; only http(s) document URLs are allowed")
	}
	rel, err := safeDocRel(src)
	if err != nil {
		return "", err
	}
	abs, err := safeDocPath(docsDir, rel)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(abs) // follows symlinks; safeDocPath already rejected escapes
	if err != nil {
		return "", fmt.Errorf("document not found under the docs dir: %s", rel)
	}
	if fi.IsDir() {
		return "", fmt.Errorf("document path is a directory: %s", rel)
	}
	// URL-encode each path segment so spaces/specials survive the fetch.
	return "/docfs/" + urlEscapePath(rel), nil
}

// safeDocRel cleans a caller-supplied relative document path and rejects
// absolute paths and traversal that would escape the docs root.
func safeDocRel(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	rel = strings.TrimPrefix(rel, "/docfs/")
	if rel == "" {
		return "", fmt.Errorf("empty document path")
	}
	if strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("document path must be relative (no leading /)")
	}
	clean := filepath.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return "", fmt.Errorf("document path escapes the docs dir")
	}
	return clean, nil
}

// safeDocPath joins rel under docsDir and verifies (via the real, symlink-
// resolved paths) that the result stays inside the docs root — defeating a
// symlink that points outside.
func safeDocPath(docsDir, rel string) (string, error) {
	abs := filepath.Join(docsDir, rel)
	// Resolve symlinks on the deepest existing ancestor and re-check containment.
	realRoot, err := filepath.EvalSymlinks(docsDir)
	if err != nil {
		realRoot = filepath.Clean(docsDir)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	rootWithSep := realRoot + string(filepath.Separator)
	if abs != realRoot && !strings.HasPrefix(abs, rootWithSep) {
		return "", fmt.Errorf("document path escapes the docs dir")
	}
	return abs, nil
}

// urlEscapePath escapes each segment of a relative path for use in a URL.
func urlEscapePath(rel string) string {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// withPDFViewerParams appends a viewer hash to a document URL that hides the
// built-in PDF viewer chrome (toolbar/navpanes), so a PDF fills the kiosk. It
// never clobbers an existing #fragment.
func withPDFViewerParams(u string) string {
	if u == "" || strings.Contains(u, "#") {
		return u
	}
	return u + "#toolbar=0&navpanes=0&scrollbar=0&view=FitH"
}
