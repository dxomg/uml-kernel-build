CC      ?= gcc
CFLAGS  ?= -Wall -Wextra -Wpedantic -O2 -g
LDFLAGS ?=

TARGET  := slirp
SRC     := slirp.c

# ── Static build paths (adjust if your libvdeslirp checkout is elsewhere) ──
VDESLIRP_SRC   ?= /tmp/libvdeslirp
VDESLIRP_BUILD ?= $(VDESLIRP_SRC)/build
SYSLIB         := /usr/lib/x86_64-linux-gnu

# Prefix of a locally built libslirp (see .github/workflows/slirp.yml). CI
# builds it with NB_BOOTP_CLIENTS raised so nested hypervisor guests can hand
# out more than 16 DHCP leases. Empty = use the distro libslirp.
SLIRP_PREFIX   ?=
ifneq ($(SLIRP_PREFIX),)
  SLIRP_INC := -I$(SLIRP_PREFIX)/include
  SLIRP_LIB := $(firstword $(wildcard $(SLIRP_PREFIX)/lib/*/libslirp.a $(SLIRP_PREFIX)/lib/libslirp.a))
else
  SLIRP_INC := -I/usr/include/slirp
  SLIRP_LIB := $(SYSLIB)/libslirp.a
endif

.PHONY: all static clean

# ── Default: dynamic link ──
all: $(TARGET)

$(TARGET): $(SRC)
	$(CC) $(CFLAGS) $(LDFLAGS) -o $@ $< -lvdeslirp -lyaml

# ── Static: fully portable binary ──
static: $(SRC) $(VDESLIRP_BUILD)/libvdeslirp.a
	$(CC) $(CFLAGS) -static -DSTATIC_BUILD \
		-I$(VDESLIRP_SRC) -I$(VDESLIRP_BUILD) $(SLIRP_INC) \
		-o $(TARGET) $< \
		$(VDESLIRP_BUILD)/libvdeslirp.a \
		$(SYSLIB)/libyaml.a \
		$(SLIRP_LIB) \
		$(SYSLIB)/libglib-2.0.a \
		$(SYSLIB)/libpcre.a \
		-pthread -lm
	@echo "=== Static build complete ==="
	@file $(TARGET)
	@ls -lh $(TARGET)

clean:
	rm -f $(TARGET)
