// disp-kmsshot — universal DRM/KMS screenshot for sideshow nodes.
//
// Captures whatever a CRTC is *currently scanning out* — below X11, Wayland,
// the cog/KMS kiosk, a direct-KMS A/V app, or a bare console — by reading the
// active framebuffer(s) over the DRM API and GPU-detiling them through EGL.
// This is the one backend-agnostic path: every display mode ends as a
// framebuffer bound to a KMS plane, and drmModeGetFB2 hands it to us regardless
// of who drew it. The GPU import (EGLImage from the dma-buf, honoring the format
// modifier) detiles for free, so the same code works on linear (vc4 under X) and
// tiled (i915) buffers without a per-modifier CPU detiler.
//
// Output: binary PPM (P6) on stdout. The sideshow agent execs this, parses the
// PPM, and does PNG/scaling/serving in Go — keeping the agent pure-Go and
// cross-compilable while this helper owns the GPU bits.
//
// Privilege: drmModeGetFB2 returns the buffer handles only to the DRM master or
// a CAP_SYS_ADMIN caller, so this must run as root (the agent does). The GL side
// uses the render node and needs no master.
//
// By default it composites ALL active planes on the CRTC in zpos order (primary
// + video overlays + hardware cursor) for a true "what's on screen" capture;
// --primary restricts to the primary plane.
//
//   disp-kmsshot [-D /dev/dri/card0] [-r /dev/dri/renderD128]
//                [-c <crtc_id>] [-p <plane_id>] [--primary] [-d] > shot.ppm
//
// SPDX-License-Identifier: MIT  (c) 2026 sideshow

#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <stdarg.h>
#include <fcntl.h>
#include <unistd.h>
#include <errno.h>

#include <xf86drm.h>
#include <xf86drmMode.h>
#include <drm_fourcc.h>
#include <gbm.h>

#include <EGL/egl.h>
#include <EGL/eglext.h>
#include <GLES2/gl2.h>
#include <GLES2/gl2ext.h>

// ---- diagnostics -----------------------------------------------------------

static void die(const char *fmt, ...) {
	va_list ap;
	va_start(ap, fmt);
	fprintf(stderr, "kmsshot: ");
	vfprintf(stderr, fmt, ap);
	fputc('\n', stderr);
	va_end(ap);
	exit(1);
}

// ---- DRM property helpers --------------------------------------------------

// prop reads a single named property value off a KMS object. Returns 0 on hit,
// -1 if the object has no such property (so callers can fall back).
static int prop(int fd, uint32_t id, uint32_t type, const char *name, uint64_t *out) {
	drmModeObjectProperties *props = drmModeObjectGetProperties(fd, id, type);
	if (!props)
		return -1;
	int ret = -1;
	for (uint32_t i = 0; i < props->count_props && ret; i++) {
		drmModePropertyRes *p = drmModeGetProperty(fd, props->props[i]);
		if (!p)
			continue;
		if (!strcmp(p->name, name)) {
			*out = props->prop_values[i];
			ret = 0;
		}
		drmModeFreeProperty(p);
	}
	drmModeFreeObjectProperties(props);
	return ret;
}

// A plane we've decided to draw, with its placement resolved from KMS props.
struct grab_plane {
	uint32_t plane_id, fb_id;
	int64_t zkey;                 // sort key: zpos if present, else type rank
	int dx, dy, dw, dh;           // destination rect on the CRTC (CRTC_* props)
	double sx, sy, sw, sh;        // source rect in the FB (SRC_* props, 16.16→px)
};

static int cmp_plane(const void *a, const void *b) {
	int64_t za = ((const struct grab_plane *)a)->zkey;
	int64_t zb = ((const struct grab_plane *)b)->zkey;
	return (za > zb) - (za < zb);
}

// ---- EGL / GLES ------------------------------------------------------------

static const char *vert_src =
	"attribute vec2 a_pos;\n"
	"attribute vec2 a_uv;\n"
	"varying vec2 v_uv;\n"
	"void main() { v_uv = a_uv; gl_Position = vec4(a_pos, 0.0, 1.0); }\n";

// Samples the dma-buf as an external texture (the driver does any YUV→RGB and
// detiling). force_opaque pins alpha to 1 for the bottom/primary plane, whose
// XR24 alpha channel is meaningless; overlays/cursor blend with real alpha.
static const char *frag_src =
	"#extension GL_OES_EGL_image_external : require\n"
	"precision mediump float;\n"
	"uniform samplerExternalOES u_tex;\n"
	"uniform float u_opaque;\n"
	"varying vec2 v_uv;\n"
	"void main() {\n"
	"  vec4 c = texture2D(u_tex, v_uv);\n"
	"  gl_FragColor = vec4(c.rgb, mix(c.a, 1.0, u_opaque));\n"
	"}\n";

static GLuint compile(GLenum kind, const char *src) {
	GLuint sh = glCreateShader(kind);
	glShaderSource(sh, 1, &src, NULL);
	glCompileShader(sh);
	GLint ok = 0;
	glGetShaderiv(sh, GL_COMPILE_STATUS, &ok);
	if (!ok) {
		char log[1024] = {0};
		glGetShaderInfoLog(sh, sizeof log, NULL, log);
		die("shader compile: %s", log);
	}
	return sh;
}

static PFNEGLCREATEIMAGEKHRPROC eglCreateImageKHR_;
static PFNEGLDESTROYIMAGEKHRPROC eglDestroyImageKHR_;
static PFNGLEGLIMAGETARGETTEXTURE2DOESPROC glEGLImageTargetTexture2DOES_;

// num_fb_planes counts the populated buffer planes of a getFB2 framebuffer
// (NV12 etc. carry 2+). drmModeGetFB2 zeroes the handles of unused planes.
static int num_fb_planes(const drmModeFB2 *fb) {
	int n = 0;
	for (int i = 0; i < 4; i++)
		if (fb->handles[i])
			n++;
	return n ? n : 1;
}

// import_fb turns a scanout framebuffer into a GL external texture via an
// EGLImage built from its dma-buf(s), honoring the format modifier so the GPU
// detiles. Returns the texture; *img must be destroyed by the caller.
static GLuint import_fb(int card_fd, EGLDisplay dpy, const drmModeFB2 *fb,
                        int have_mod_ext, EGLImageKHR *img) {
	static const EGLint FD[4]     = {EGL_DMA_BUF_PLANE0_FD_EXT, EGL_DMA_BUF_PLANE1_FD_EXT,
	                                 EGL_DMA_BUF_PLANE2_FD_EXT, EGL_DMA_BUF_PLANE3_FD_EXT};
	static const EGLint OFFSET[4] = {EGL_DMA_BUF_PLANE0_OFFSET_EXT, EGL_DMA_BUF_PLANE1_OFFSET_EXT,
	                                 EGL_DMA_BUF_PLANE2_OFFSET_EXT, EGL_DMA_BUF_PLANE3_OFFSET_EXT};
	static const EGLint PITCH[4]  = {EGL_DMA_BUF_PLANE0_PITCH_EXT, EGL_DMA_BUF_PLANE1_PITCH_EXT,
	                                 EGL_DMA_BUF_PLANE2_PITCH_EXT, EGL_DMA_BUF_PLANE3_PITCH_EXT};
	static const EGLint MODLO[4]  = {EGL_DMA_BUF_PLANE0_MODIFIER_LO_EXT, EGL_DMA_BUF_PLANE1_MODIFIER_LO_EXT,
	                                 EGL_DMA_BUF_PLANE2_MODIFIER_LO_EXT, EGL_DMA_BUF_PLANE3_MODIFIER_LO_EXT};
	static const EGLint MODHI[4]  = {EGL_DMA_BUF_PLANE0_MODIFIER_HI_EXT, EGL_DMA_BUF_PLANE1_MODIFIER_HI_EXT,
	                                 EGL_DMA_BUF_PLANE2_MODIFIER_HI_EXT, EGL_DMA_BUF_PLANE3_MODIFIER_HI_EXT};

	int np = num_fb_planes(fb);
	int use_mod = have_mod_ext && fb->modifier != DRM_FORMAT_MOD_INVALID;

	// Export each distinct GEM handle to a dma-buf fd (planes may share one).
	int dfd[4] = {-1, -1, -1, -1};
	for (int i = 0; i < np; i++) {
		for (int j = 0; j < i; j++) {
			if (fb->handles[j] == fb->handles[i]) {
				dfd[i] = dfd[j];
				break;
			}
		}
		if (dfd[i] < 0 &&
		    drmPrimeHandleToFD(card_fd, fb->handles[i], O_RDWR | O_CLOEXEC, &dfd[i]) != 0)
			die("PrimeHandleToFD (handle %u): %s", fb->handles[i], strerror(errno));
	}

	EGLint a[64];
	int n = 0;
	a[n++] = EGL_WIDTH;             a[n++] = fb->width;
	a[n++] = EGL_HEIGHT;           a[n++] = fb->height;
	a[n++] = EGL_LINUX_DRM_FOURCC_EXT; a[n++] = (EGLint)fb->pixel_format;
	for (int i = 0; i < np; i++) {
		a[n++] = FD[i];     a[n++] = dfd[i];
		a[n++] = OFFSET[i]; a[n++] = (EGLint)fb->offsets[i];
		a[n++] = PITCH[i];  a[n++] = (EGLint)fb->pitches[i];
		if (use_mod) {
			a[n++] = MODLO[i]; a[n++] = (EGLint)(fb->modifier & 0xffffffff);
			a[n++] = MODHI[i]; a[n++] = (EGLint)(fb->modifier >> 32);
		}
	}
	a[n++] = EGL_NONE;

	*img = eglCreateImageKHR_(dpy, EGL_NO_CONTEXT, EGL_LINUX_DMA_BUF_EXT, NULL, a);
	// The fds are dup'd into the EGLImage; close ours.
	for (int i = 0; i < np; i++) {
		int dup_of = 0;
		for (int j = 0; j < i; j++)
			if (dfd[j] == dfd[i]) { dup_of = 1; break; }
		if (!dup_of && dfd[i] >= 0)
			close(dfd[i]);
	}
	if (*img == EGL_NO_IMAGE_KHR)
		die("eglCreateImageKHR failed (fourcc %.4s modifier 0x%llx) — driver can't "
		    "import this scanout buffer", (char *)&fb->pixel_format,
		    (unsigned long long)fb->modifier);

	GLuint tex = 0;
	glGenTextures(1, &tex);
	glBindTexture(GL_TEXTURE_EXTERNAL_OES, tex);
	glTexParameteri(GL_TEXTURE_EXTERNAL_OES, GL_TEXTURE_MIN_FILTER, GL_LINEAR);
	glTexParameteri(GL_TEXTURE_EXTERNAL_OES, GL_TEXTURE_MAG_FILTER, GL_LINEAR);
	glTexParameteri(GL_TEXTURE_EXTERNAL_OES, GL_TEXTURE_WRAP_S, GL_CLAMP_TO_EDGE);
	glTexParameteri(GL_TEXTURE_EXTERNAL_OES, GL_TEXTURE_WRAP_T, GL_CLAMP_TO_EDGE);
	glEGLImageTargetTexture2DOES_(GL_TEXTURE_EXTERNAL_OES, *img);
	return tex;
}

static int has_ext(const char *exts, const char *name) {
	return exts && strstr(exts, name) != NULL;
}

int main(int argc, char **argv) {
	const char *card = "/dev/dri/card0";
	const char *render = NULL;          // default: derive from card's render node
	uint32_t want_crtc = 0, want_plane = 0;
	int primary_only = 0, dump = 0;

	for (int i = 1; i < argc; i++) {
		if (!strcmp(argv[i], "-D") && i + 1 < argc) card = argv[++i];
		else if (!strcmp(argv[i], "-r") && i + 1 < argc) render = argv[++i];
		else if (!strcmp(argv[i], "-c") && i + 1 < argc) want_crtc = strtoul(argv[++i], NULL, 0);
		else if (!strcmp(argv[i], "-p") && i + 1 < argc) { want_plane = strtoul(argv[++i], NULL, 0); primary_only = 1; }
		else if (!strcmp(argv[i], "--primary")) primary_only = 1;
		else if (!strcmp(argv[i], "-d")) dump = 1;
		else die("usage: disp-kmsshot [-D card] [-r render] [-c crtc] [-p plane] [--primary] [-d]");
	}

	int fd = open(card, O_RDWR | O_CLOEXEC);
	if (fd < 0)
		die("open %s: %s (run as root)", card, strerror(errno));
	// See overlay + cursor planes, not just the primary.
	drmSetClientCap(fd, DRM_CLIENT_CAP_UNIVERSAL_PLANES, 1);
	drmSetClientCap(fd, DRM_CLIENT_CAP_ATOMIC, 1); // exposes zpos/SRC_/CRTC_ props

	drmModeRes *res = drmModeGetResources(fd);
	if (!res)
		die("drmModeGetResources: %s (is %s a KMS device?)", strerror(errno), card);

	// Pick the CRTC. We can't trust the legacy crtc->buffer_id: under an atomic
	// driver (vc4, or i915 with a Wayland/atomic compositor) it's 0 even when the
	// display is live, because the scanout FB hangs off the PRIMARY PLANE, not the
	// legacy CRTC field. So locate the CRTC via a framebuffer-bearing plane — the
	// way kmsgrab finds scanout — and fall back to any modeset CRTC.
	uint32_t target = want_crtc;
	if (!target) {
		drmModePlaneRes *pr = drmModeGetPlaneResources(fd);
		if (pr) {
			for (uint32_t i = 0; i < pr->count_planes && !target; i++) {
				drmModePlane *pl = drmModeGetPlane(fd, pr->planes[i]);
				if (pl) {
					if (pl->crtc_id && pl->fb_id)
						target = pl->crtc_id;
					drmModeFreePlane(pl);
				}
			}
			drmModeFreePlaneResources(pr);
		}
	}
	if (!target) { // nothing on a plane — fall back to any CRTC with a set mode
		for (int i = 0; i < res->count_crtcs && !target; i++) {
			drmModeCrtc *c = drmModeGetCrtc(fd, res->crtcs[i]);
			if (c) {
				if (c->mode_valid)
					target = c->crtc_id;
				drmModeFreeCrtc(c);
			}
		}
	}
	if (!target)
		die("no CRTC is scanning out (display off or asleep?)");
	drmModeCrtc *crtc = drmModeGetCrtc(fd, target);
	if (!crtc)
		die("drmModeGetCrtc(%u): %s", target, strerror(errno));
	int cw = crtc->width, ch = crtc->height;
	if (cw <= 0 || ch <= 0)
		die("CRTC %u has no valid mode", crtc->crtc_id);

	// Gather the planes to draw, bound to our CRTC and carrying a framebuffer.
	drmModePlaneRes *planes = drmModeGetPlaneResources(fd);
	if (!planes)
		die("drmModeGetPlaneResources: %s", strerror(errno));
	struct grab_plane grab[32];
	int ng = 0;
	for (uint32_t i = 0; i < planes->count_planes && ng < 32; i++) {
		drmModePlane *pl = drmModeGetPlane(fd, planes->planes[i]);
		if (!pl)
			continue;
		int mine = pl->crtc_id == crtc->crtc_id && pl->fb_id &&
		           (!want_plane || pl->plane_id == want_plane);
		if (!mine) { drmModeFreePlane(pl); continue; }

		struct grab_plane g = {.plane_id = pl->plane_id, .fb_id = pl->fb_id};
		// Placement from atomic props; fall back to fullscreen if absent.
		uint64_t v;
		g.dx = prop(fd, pl->plane_id, DRM_MODE_OBJECT_PLANE, "CRTC_X", &v) ? 0 : (int32_t)v;
		g.dy = prop(fd, pl->plane_id, DRM_MODE_OBJECT_PLANE, "CRTC_Y", &v) ? 0 : (int32_t)v;
		g.dw = prop(fd, pl->plane_id, DRM_MODE_OBJECT_PLANE, "CRTC_W", &v) ? cw : (int)v;
		g.dh = prop(fd, pl->plane_id, DRM_MODE_OBJECT_PLANE, "CRTC_H", &v) ? ch : (int)v;
		g.sx = prop(fd, pl->plane_id, DRM_MODE_OBJECT_PLANE, "SRC_X", &v) ? 0 : v / 65536.0;
		g.sy = prop(fd, pl->plane_id, DRM_MODE_OBJECT_PLANE, "SRC_Y", &v) ? 0 : v / 65536.0;
		g.sw = prop(fd, pl->plane_id, DRM_MODE_OBJECT_PLANE, "SRC_W", &v) ? 0 : v / 65536.0;
		g.sh = prop(fd, pl->plane_id, DRM_MODE_OBJECT_PLANE, "SRC_H", &v) ? 0 : v / 65536.0;
		// Sort key: explicit zpos if the driver exposes it, else by plane type
		// (primary below, overlay, cursor on top).
		if (!prop(fd, pl->plane_id, DRM_MODE_OBJECT_PLANE, "zpos", &v)) {
			g.zkey = (int64_t)v;
		} else {
			uint64_t t = 1; // default overlay
			prop(fd, pl->plane_id, DRM_MODE_OBJECT_PLANE, "type", &t);
			g.zkey = t == DRM_PLANE_TYPE_PRIMARY ? -1000
			       : t == DRM_PLANE_TYPE_CURSOR  ?  1000 : 0;
		}
		grab[ng++] = g;
		drmModeFreePlane(pl);
	}
	drmModeFreePlaneResources(planes);
	if (ng == 0)
		die("no active plane on CRTC %u", crtc->crtc_id);
	qsort(grab, ng, sizeof grab[0], cmp_plane);
	if (primary_only)
		ng = 1; // lowest zkey == primary, after the sort

	if (dump) {
		fprintf(stderr, "CRTC %u  %dx%d  planes=%d\n", crtc->crtc_id, cw, ch, ng);
		for (int i = 0; i < ng; i++) {
			drmModeFB2 *fb = drmModeGetFB2(fd, grab[i].fb_id);
			if (fb) {
				fprintf(stderr, "  plane %u fb %u  %ux%u  %.4s  mod 0x%llx  z=%lld  "
				        "dst %d,%d %dx%d\n", grab[i].plane_id, grab[i].fb_id,
				        fb->width, fb->height, (char *)&fb->pixel_format,
				        (unsigned long long)fb->modifier, (long long)grab[i].zkey,
				        grab[i].dx, grab[i].dy, grab[i].dw, grab[i].dh);
				drmModeFreeFB2(fb);
			}
		}
		return 0;
	}

	// ---- EGL on the render node (no DRM master needed for GL) ----
	int rfd = -1;
	if (render) {
		rfd = open(render, O_RDWR | O_CLOEXEC);
		if (rfd < 0)
			die("open %s: %s", render, strerror(errno));
	} else {
		char *rn = drmGetRenderDeviceNameFromFd(fd);
		rfd = rn ? open(rn, O_RDWR | O_CLOEXEC) : -1;
		free(rn);
		if (rfd < 0) // some SoCs share one node for display + render
			rfd = open(card, O_RDWR | O_CLOEXEC);
	}
	struct gbm_device *gbm = gbm_create_device(rfd);
	if (!gbm)
		die("gbm_create_device on render node failed");

	PFNEGLGETPLATFORMDISPLAYEXTPROC getPlatformDisplay =
		(void *)eglGetProcAddress("eglGetPlatformDisplayEXT");
	EGLDisplay dpy = getPlatformDisplay
		? getPlatformDisplay(EGL_PLATFORM_GBM_KHR, gbm, NULL)
		: eglGetDisplay((EGLNativeDisplayType)gbm);
	if (dpy == EGL_NO_DISPLAY)
		die("eglGetDisplay(GBM) failed");
	if (!eglInitialize(dpy, NULL, NULL))
		die("eglInitialize failed");

	const char *exts = eglQueryString(dpy, EGL_EXTENSIONS);
	if (!has_ext(exts, "EGL_EXT_image_dma_buf_import"))
		die("EGL_EXT_image_dma_buf_import unsupported on this GPU");
	int have_mod_ext = has_ext(exts, "EGL_EXT_image_dma_buf_import_modifiers");

	eglCreateImageKHR_ = (void *)eglGetProcAddress("eglCreateImageKHR");
	eglDestroyImageKHR_ = (void *)eglGetProcAddress("eglDestroyImageKHR");
	glEGLImageTargetTexture2DOES_ = (void *)eglGetProcAddress("glEGLImageTargetTexture2DOES");
	if (!eglCreateImageKHR_ || !glEGLImageTargetTexture2DOES_)
		die("dma-buf import entry points missing");

	if (!eglBindAPI(EGL_OPENGL_ES_API))
		die("eglBindAPI(GLES) failed");
	// We only ever render to an FBO (no EGL surface), so prefer a config-less
	// context — EGL_KHR_no_config_context sidesteps the GBM-platform gotcha where
	// no config advertises a pbuffer. Fall back to any ES2-renderable config.
	EGLConfig cfg = EGL_NO_CONFIG_KHR;
	if (!has_ext(exts, "EGL_KHR_no_config_context")) {
		EGLint cfg_attr[] = {EGL_SURFACE_TYPE, EGL_WINDOW_BIT,
		                     EGL_RENDERABLE_TYPE, EGL_OPENGL_ES2_BIT,
		                     EGL_RED_SIZE, 8, EGL_GREEN_SIZE, 8, EGL_BLUE_SIZE, 8,
		                     EGL_NONE};
		EGLint ncfg = 0;
		if (!eglChooseConfig(dpy, cfg_attr, &cfg, 1, &ncfg) || ncfg == 0)
			die("eglChooseConfig found no ES2 config");
	}
	EGLint ctx_attr[] = {EGL_CONTEXT_CLIENT_VERSION, 2, EGL_NONE};
	EGLContext ctx = eglCreateContext(dpy, cfg, EGL_NO_CONTEXT, ctx_attr);
	if (ctx == EGL_NO_CONTEXT)
		die("eglCreateContext failed");
	if (!eglMakeCurrent(dpy, EGL_NO_SURFACE, EGL_NO_SURFACE, ctx))
		die("eglMakeCurrent(surfaceless) failed — no EGL_KHR_surfaceless_context?");

	// Destination: an RGBA texture the size of the CRTC, wrapped in an FBO.
	GLuint dst = 0, fbo = 0;
	glGenTextures(1, &dst);
	glBindTexture(GL_TEXTURE_2D, dst);
	glTexImage2D(GL_TEXTURE_2D, 0, GL_RGBA, cw, ch, 0, GL_RGBA, GL_UNSIGNED_BYTE, NULL);
	glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MIN_FILTER, GL_NEAREST);
	glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MAG_FILTER, GL_NEAREST);
	glGenFramebuffers(1, &fbo);
	glBindFramebuffer(GL_FRAMEBUFFER, fbo);
	glFramebufferTexture2D(GL_FRAMEBUFFER, GL_COLOR_ATTACHMENT0, GL_TEXTURE_2D, dst, 0);
	if (glCheckFramebufferStatus(GL_FRAMEBUFFER) != GL_FRAMEBUFFER_COMPLETE)
		die("destination FBO incomplete");
	glViewport(0, 0, cw, ch);
	glClearColor(0, 0, 0, 1);
	glClear(GL_COLOR_BUFFER_BIT);

	// Shader program.
	GLuint prog = glCreateProgram();
	glAttachShader(prog, compile(GL_VERTEX_SHADER, vert_src));
	glAttachShader(prog, compile(GL_FRAGMENT_SHADER, frag_src));
	glBindAttribLocation(prog, 0, "a_pos");
	glBindAttribLocation(prog, 1, "a_uv");
	glLinkProgram(prog);
	GLint linked = 0;
	glGetProgramiv(prog, GL_LINK_STATUS, &linked);
	if (!linked)
		die("program link failed");
	glUseProgram(prog);
	GLint u_opaque = glGetUniformLocation(prog, "u_opaque");
	glEnableVertexAttribArray(0);
	glEnableVertexAttribArray(1);

	// Composite planes bottom→top. The first (primary) is drawn opaque with no
	// blend; overlays/cursor blend with their real alpha.
	for (int i = 0; i < ng; i++) {
		drmModeFB2 *fb = drmModeGetFB2(fd, grab[i].fb_id);
		if (!fb)
			die("drmModeGetFB2(%u) failed: %s (need root/CAP_SYS_ADMIN)",
			    grab[i].fb_id, strerror(errno));
		// Default a zero/absent SRC rect to the whole framebuffer.
		double sw = grab[i].sw > 0 ? grab[i].sw : fb->width;
		double sh = grab[i].sh > 0 ? grab[i].sh : fb->height;

		EGLImageKHR img;
		GLuint tex = import_fb(fd, dpy, fb, have_mod_ext, &img);

		// Destination rect → NDC (screen-top maps to +Y). Source rect → UV
		// (v=0 at the top, matching the scanout's top-left origin).
		double x0 = (double)grab[i].dx / cw * 2 - 1;
		double x1 = (double)(grab[i].dx + grab[i].dw) / cw * 2 - 1;
		double yt = 1 - (double)grab[i].dy / ch * 2;
		double yb = 1 - (double)(grab[i].dy + grab[i].dh) / ch * 2;
		double u0 = grab[i].sx / fb->width,  u1 = (grab[i].sx + sw) / fb->width;
		double v0 = grab[i].sy / fb->height, v1 = (grab[i].sy + sh) / fb->height;
		GLfloat verts[] = {
			(float)x0, (float)yt, (float)u0, (float)v0,
			(float)x1, (float)yt, (float)u1, (float)v0,
			(float)x0, (float)yb, (float)u0, (float)v1,
			(float)x1, (float)yb, (float)u1, (float)v1,
		};
		glVertexAttribPointer(0, 2, GL_FLOAT, GL_FALSE, 4 * sizeof(GLfloat), verts);
		glVertexAttribPointer(1, 2, GL_FLOAT, GL_FALSE, 4 * sizeof(GLfloat), verts + 2);

		if (i == 0) {
			glDisable(GL_BLEND);
			glUniform1f(u_opaque, 1.0f);
		} else {
			glEnable(GL_BLEND);
			glBlendFunc(GL_SRC_ALPHA, GL_ONE_MINUS_SRC_ALPHA);
			glUniform1f(u_opaque, 0.0f);
		}
		glDrawArrays(GL_TRIANGLE_STRIP, 0, 4);

		glDeleteTextures(1, &tex);
		eglDestroyImageKHR_(dpy, img);
		drmModeFreeFB2(fb);
	}
	glFinish();

	// Read back RGBA. glReadPixels is bottom-up; emit PPM rows top-down.
	unsigned char *rgba = malloc((size_t)cw * ch * 4);
	if (!rgba)
		die("out of memory");
	glReadPixels(0, 0, cw, ch, GL_RGBA, GL_UNSIGNED_BYTE, rgba);
	if (glGetError() != GL_NO_ERROR)
		die("glReadPixels failed");

	printf("P6\n%d %d\n255\n", cw, ch);
	unsigned char *row = malloc((size_t)cw * 3);
	if (!row)
		die("out of memory");
	for (int y = ch - 1; y >= 0; y--) {
		const unsigned char *src = rgba + (size_t)y * cw * 4;
		for (int x = 0; x < cw; x++) {
			row[x * 3 + 0] = src[x * 4 + 0];
			row[x * 3 + 1] = src[x * 4 + 1];
			row[x * 3 + 2] = src[x * 4 + 2];
		}
		if (fwrite(row, 1, (size_t)cw * 3, stdout) != (size_t)cw * 3)
			die("short write to stdout");
	}
	fflush(stdout);
	return 0; // process exit reclaims GL/DRM resources
}
