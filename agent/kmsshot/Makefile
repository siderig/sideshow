# disp-kmsshot — universal DRM/KMS screenshot helper for sideshow nodes.
# Built ON the target node (against its real Mesa/libdrm), not cross-compiled:
# the sideshow agent stays pure-Go; this is the only piece that links the GPU
# stack (EGL/GLES2/GBM). See scripts/disp-deploy.sh `kmsshot`.

PKGS  = libdrm egl glesv2 gbm
CC   ?= cc
CFLAGS ?= -O2 -Wall -Wextra
CFLAGS += $(shell pkg-config --cflags $(PKGS))
LDLIBS += $(shell pkg-config --libs $(PKGS))

PREFIX ?= /usr/local
BIN     = disp-kmsshot

$(BIN): kmsshot.c
	$(CC) $(CFLAGS) -o $@ kmsshot.c $(LDLIBS)

install: $(BIN)
	install -m 0755 $(BIN) $(DESTDIR)$(PREFIX)/bin/$(BIN)

clean:
	rm -f $(BIN)

.PHONY: install clean
