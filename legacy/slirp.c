/*
 *  slirp — Minimal SLIP↔Ethernet bridge for User Mode Linux
 *
 *  Based on libvdeslirp <https://github.com/virtualsquare/libvdeslirp>
 *
 *  Usage with UML:
 *      ./vmlinux ... eth0=slirp,,/path/to/slirp
 *      # ip a add dev eth0 10.0.2.1/24 && ip l set eth0 up
 *      # ip r add default dev eth0
 *      # echo nameserver 10.0.2.3 >/etc/resolv.conf
 *
 *  Configuration via slirp.yaml (searched next to binary, then CWD):
 *      tcp: true
 *      udp: true
 *      portfwd: true
 *      ports:
 *        - 2222:22
 *        - 8080:80
 *        - udp 5353:53
 *
 *  Build:  make
 *  Flags:  -v  verbose logging
 */

#include <stdio.h>
#include <string.h>
#include <stdint.h>
#include <stdlib.h>
#include <stdbool.h>
#include <unistd.h>
#include <poll.h>
#include <fcntl.h>
#include <signal.h>
#include <errno.h>
#include <getopt.h>
#include <arpa/inet.h>
#include <sys/uio.h>
#include <limits.h>
#include <time.h>

#include <yaml.h>

/*
 * libvdeslirp header location varies:
 *   - system package: /usr/include/slirp/libvdeslirp.h
 *   - static build:   passed via -I to the source directory
 * We handle both with a conditional include.
 */
#ifdef STATIC_BUILD
#include <libvdeslirp.h>
#else
#include <slirp/libvdeslirp.h>
#endif

/* ── SLIP protocol (RFC 1055) ────────────────────────────────────── */

#define SLIP_END      0300
#define SLIP_ESC      0333
#define SLIP_ESC_END  0334
#define SLIP_ESC_ESC  0335

#define SLIP_END_S    "\300"

/* ── Tunables ────────────────────────────────────────────────────── */

#define MAXPKT        1600
#define SLIP_MAXPKT   ((MAXPKT) * 2 + 2)

/* Guest identity inside the slirp network */
#define MYIP          "\x0a\x00\x02\x01"
#define MYMAC         "\xaa\xaa\xaa\xaa\xaa\xaa"
#define GATEWAYMAC    "\x52\x55\x0a\x00\x02\x02"

/* ── Global state ────────────────────────────────────────────────── */

static volatile sig_atomic_t g_running = 1;
static bool g_verbose = false;

/* Stats counters */
static uint64_t g_stat_tx_packets = 0;   /* slip → slirp */
static uint64_t g_stat_rx_packets = 0;   /* slirp → slip */
static uint64_t g_stat_tx_bytes   = 0;
static uint64_t g_stat_rx_bytes   = 0;
static uint64_t g_stat_arp_reply  = 0;
static uint64_t g_stat_drop       = 0;

/* ── YAML config ─────────────────────────────────────────────────── */

#define MAX_PORT_FORWARDS 256

typedef struct {
    bool     is_udp;
    uint16_t host_port;
    uint16_t guest_port;
} port_forward_t;

typedef struct {
    bool           tcp_enabled;
    bool           udp_enabled;
    bool           portfwd_enabled;
    size_t         num_forwards;
    port_forward_t forwards[MAX_PORT_FORWARDS];
} slirp_config_t;

/* ── SLIP receive state machine ──────────────────────────────────── */

/*
 * SLIP receive buffer layout:
 * The raw[] array contains: [14 bytes headroom][SLIP_MAXPKT data][14 bytes tailroom]
 * This allows in-place Ethernet header prepend without extra copying.
 */
#define SLIP_RAW_SIZE  (14 + SLIP_MAXPKT + 14)

typedef struct {
    uint8_t raw[SLIP_RAW_SIZE];    /* headroom + data + tailroom */
    uint8_t scratch[MAXPKT];
    size_t  index;                 /* bytes in data portion */
    size_t  discard;
} slip_recv_state_t;

/* Data portion starts at offset 14 in raw[] */
#define SLIP_BUF(s) ((s)->raw + 14)

/* ── Logging ─────────────────────────────────────────────────────── */

#define LOG_ERR(fmt, ...)  fprintf(stderr, "slirp: error: " fmt "\n", ##__VA_ARGS__)
#define LOG_WARN(fmt, ...) fprintf(stderr, "slirp: warn: " fmt "\n", ##__VA_ARGS__)
#define LOG_INFO(fmt, ...) fprintf(stderr, "slirp: " fmt "\n", ##__VA_ARGS__)
#define LOG_DBG(fmt, ...)  do { if (g_verbose) fprintf(stderr, "slirp: debug: " fmt "\n", ##__VA_ARGS__); } while(0)

/* ── Forward declarations ────────────────────────────────────────── */

static void slip_send(int fd, const uint8_t *pkt, size_t pktlen);
static uint8_t *slip_recv(int fd, slip_recv_state_t *s, size_t *pktlen);
static void slip_to_outside(struct vdeslirp *myslirp, uint8_t *pkt, size_t pktlen);
static void outside_to_slip(struct vdeslirp *myslirp, const uint8_t *pkt, size_t pktlen);
static int  load_yaml_config(const char *exe_path, slirp_config_t *cfg);
static void apply_port_forwards(struct vdeslirp *myslirp, const slirp_config_t *cfg);
static void print_stats(void);

/* ── Robust I/O helpers ──────────────────────────────────────────── */

/*
 * Write all bytes to fd, retrying on partial writes and EINTR.
 * Returns 0 on success, -1 on unrecoverable error.
 */
static int write_all(int fd, const uint8_t *buf, size_t len)
{
    while (len > 0) {
        ssize_t n = write(fd, buf, len);
        if (n > 0) {
            buf += n;
            len -= (size_t)n;
        } else if (n == -1) {
            if (errno == EINTR)
                continue;
            if (errno == EAGAIN || errno == EWOULDBLOCK) {
                /* stdout shouldn't be non-blocking, but handle it */
                struct pollfd pfd = { .fd = fd, .events = POLLOUT };
                poll(&pfd, 1, 50);
                continue;
            }
            return -1;
        }
    }
    return 0;
}

/*
 * writev all iovecs. Retries on partial writes.
 */
static int writev_all(int fd, struct iovec *iov, int iovcnt)
{
    while (iovcnt > 0) {
        ssize_t n = writev(fd, iov, iovcnt);
        if (n > 0) {
            /* Advance past completed iovecs */
            while (iovcnt > 0 && (size_t)n >= iov->iov_len) {
                n -= (ssize_t)iov->iov_len;
                iov++;
                iovcnt--;
            }
            if (iovcnt > 0 && n > 0) {
                iov->iov_base = (uint8_t *)iov->iov_base + n;
                iov->iov_len -= (size_t)n;
            }
        } else if (n == -1) {
            if (errno == EINTR)
                continue;
            if (errno == EAGAIN || errno == EWOULDBLOCK) {
                struct pollfd pfd = { .fd = fd, .events = POLLOUT };
                poll(&pfd, 1, 50);
                continue;
            }
            return -1;
        }
    }
    return 0;
}

/* ── Signal handling ─────────────────────────────────────────────── */

static void sig_handler(int sig)
{
    (void)sig;
    g_running = 0;
}

static void install_signal_handlers(void)
{
    struct sigaction sa;
    memset(&sa, 0, sizeof(sa));
    sa.sa_handler = sig_handler;
    sigemptyset(&sa.sa_mask);
    sa.sa_flags = 0;

    sigaction(SIGTERM, &sa, NULL);
    sigaction(SIGINT,  &sa, NULL);
    sigaction(SIGHUP,  &sa, NULL);

    /* Ignore SIGPIPE — we handle write errors explicitly */
    sa.sa_handler = SIG_IGN;
    sigaction(SIGPIPE, &sa, NULL);
}

/* ── YAML config parser ──────────────────────────────────────────── */

/*
 * Resolve path to slirp.yaml:
 *   1. Same directory as the executable
 *   2. Current working directory
 */
static FILE *find_config_file(const char *exe_path, char *resolved, size_t resolved_sz)
{
    FILE *f = NULL;

    /* Try: same directory as executable */
    const char *slash = strrchr(exe_path, '/');
    if (slash) {
        size_t dirlen = (size_t)(slash - exe_path);
        if (dirlen + sizeof("/slirp.yaml") < resolved_sz) {
            memcpy(resolved, exe_path, dirlen);
            memcpy(resolved + dirlen, "/slirp.yaml", sizeof("/slirp.yaml"));
            f = fopen(resolved, "r");
        }
    }

    /* Fallback: CWD */
    if (!f) {
        snprintf(resolved, resolved_sz, "slirp.yaml");
        f = fopen(resolved, "r");
    }

    return f;
}

/*
 * Parse a port line like "2222:22" or "udp 5353:53".
 * Default protocol depends on cfg->tcp_enabled / cfg->udp_enabled.
 */
static int parse_port_entry(const char *value, const slirp_config_t *cfg, port_forward_t *out)
{
    const char *p = value;
    bool is_udp = false;

    /* Skip whitespace */
    while (*p == ' ' || *p == '\t') p++;

    /* Check for explicit protocol prefix */
    if (strncasecmp(p, "udp ", 4) == 0 || strncasecmp(p, "udp\t", 4) == 0) {
        is_udp = true;
        p += 4;
    } else if (strncasecmp(p, "tcp ", 4) == 0 || strncasecmp(p, "tcp\t", 4) == 0) {
        is_udp = false;
        p += 4;
    }
    /* else: default to TCP */

    /* Validate protocol is enabled */
    if (is_udp && !cfg->udp_enabled) {
        LOG_WARN("config: UDP port forward '%s' ignored (udp: false)", value);
        return -1;
    }
    if (!is_udp && !cfg->tcp_enabled) {
        LOG_WARN("config: TCP port forward '%s' ignored (tcp: false)", value);
        return -1;
    }

    unsigned long hp, gp;
    if (sscanf(p, "%lu:%lu", &hp, &gp) != 2) {
        LOG_ERR("config: invalid port entry: '%s'", value);
        return -1;
    }

    if (hp == 0 || hp > 65535 || gp == 0 || gp > 65535) {
        LOG_ERR("config: port out of range: '%s'", value);
        return -1;
    }

    out->is_udp     = is_udp;
    out->host_port  = (uint16_t)hp;
    out->guest_port = (uint16_t)gp;
    return 0;
}

/*
 * Parse slirp.yaml using libyaml.
 *
 * Expected structure:
 *   tcp: true|false
 *   udp: true|false
 *   portfwd: true|false
 *   ports:
 *     - 2222:22
 *     - udp 5353:53
 */
static int load_yaml_config(const char *exe_path, slirp_config_t *cfg)
{
    /* Defaults */
    cfg->tcp_enabled    = true;
    cfg->udp_enabled    = false;
    cfg->portfwd_enabled = false;
    cfg->num_forwards   = 0;

    char resolved[PATH_MAX];
    FILE *f = find_config_file(exe_path, resolved, sizeof(resolved));
    if (!f) {
        LOG_INFO("no slirp.yaml found, using defaults");
        return 0;
    }

    LOG_INFO("loading config from %s", resolved);

    yaml_parser_t parser;
    yaml_event_t event;

    if (!yaml_parser_initialize(&parser)) {
        LOG_ERR("yaml_parser_initialize failed");
        fclose(f);
        return -1;
    }

    yaml_parser_set_input_file(&parser, f);

    /*
     * Simple state machine for our flat YAML structure:
     *   STATE_TOP       — expecting a mapping key
     *   STATE_VALUE     — expecting the value for current key
     *   STATE_PORTS_SEQ — inside the 'ports' sequence
     */
    enum { STATE_TOP, STATE_VALUE, STATE_PORTS_SEQ } state = STATE_TOP;
    char current_key[64] = {0};
    int done = 0;
    int rc = 0;

    while (!done) {
        if (!yaml_parser_parse(&parser, &event)) {
            LOG_ERR("YAML parse error at line %zu: %s",
                    parser.problem_mark.line + 1, parser.problem);
            rc = -1;
            break;
        }

        switch (event.type) {
        case YAML_STREAM_END_EVENT:
            done = 1;
            break;

        case YAML_SCALAR_EVENT: {
            const char *val = (const char *)event.data.scalar.value;

            if (state == STATE_TOP) {
                /* This is a mapping key */
                snprintf(current_key, sizeof(current_key), "%s", val);
                state = STATE_VALUE;
            } else if (state == STATE_VALUE) {
                /* This is the value for current_key */
                if (strcasecmp(current_key, "tcp") == 0) {
                    cfg->tcp_enabled = (strcasecmp(val, "true") == 0 || strcmp(val, "1") == 0);
                } else if (strcasecmp(current_key, "udp") == 0) {
                    cfg->udp_enabled = (strcasecmp(val, "true") == 0 || strcmp(val, "1") == 0);
                } else if (strcasecmp(current_key, "portfwd") == 0) {
                    cfg->portfwd_enabled = (strcasecmp(val, "true") == 0 || strcmp(val, "1") == 0);
                } else {
                    LOG_WARN("config: unknown key '%s'", current_key);
                }
                state = STATE_TOP;
            } else if (state == STATE_PORTS_SEQ) {
                /* Each scalar in the sequence is a port forward */
                if (cfg->num_forwards < MAX_PORT_FORWARDS) {
                    port_forward_t fwd;
                    if (parse_port_entry(val, cfg, &fwd) == 0)
                        cfg->forwards[cfg->num_forwards++] = fwd;
                } else {
                    LOG_WARN("config: too many port forwards (max %d)", MAX_PORT_FORWARDS);
                }
            }
            break;
        }

        case YAML_SEQUENCE_START_EVENT:
            if (state == STATE_VALUE && strcasecmp(current_key, "ports") == 0) {
                state = STATE_PORTS_SEQ;
            }
            break;

        case YAML_SEQUENCE_END_EVENT:
            if (state == STATE_PORTS_SEQ)
                state = STATE_TOP;
            break;

        default:
            break;
        }

        yaml_event_delete(&event);
    }

    yaml_parser_delete(&parser);
    fclose(f);

    /* Log parsed config */
    LOG_INFO("config: tcp=%s udp=%s portfwd=%s ports=%zu",
             cfg->tcp_enabled ? "on" : "off",
             cfg->udp_enabled ? "on" : "off",
             cfg->portfwd_enabled ? "on" : "off",
             cfg->num_forwards);

    return rc;
}

/* ── Apply port forwards to slirp ────────────────────────────────── */

static void apply_port_forwards(struct vdeslirp *myslirp, const slirp_config_t *cfg)
{
    if (!cfg->portfwd_enabled) {
        if (cfg->num_forwards > 0)
            LOG_WARN("portfwd is disabled but %zu port entries found — ignoring", cfg->num_forwards);
        return;
    }

    struct in_addr guest_ip;
    memcpy(&guest_ip.s_addr, MYIP, 4);

    for (size_t i = 0; i < cfg->num_forwards; i++) {
        const port_forward_t *fwd = &cfg->forwards[i];

        int rc = vdeslirp_add_fwd(myslirp, fwd->is_udp ? 1 : 0,
                                  (struct in_addr){ .s_addr = INADDR_ANY },
                                  fwd->host_port,
                                  guest_ip,
                                  fwd->guest_port);

        if (rc != 0) {
            LOG_ERR("forward %s host:%u → guest:%u failed (rc=%d)",
                    fwd->is_udp ? "udp" : "tcp",
                    fwd->host_port, fwd->guest_port, rc);
        } else {
            LOG_INFO("forward %s host:%u → guest:%u",
                     fwd->is_udp ? "udp" : "tcp",
                     fwd->host_port, fwd->guest_port);
        }
    }
}

/* ── Stats ───────────────────────────────────────────────────────── */

static void print_stats(void)
{
    LOG_INFO("stats: tx=%lu pkts (%lu bytes), rx=%lu pkts (%lu bytes), arp=%lu, drop=%lu",
             (unsigned long)g_stat_tx_packets, (unsigned long)g_stat_tx_bytes,
             (unsigned long)g_stat_rx_packets, (unsigned long)g_stat_rx_bytes,
             (unsigned long)g_stat_arp_reply,  (unsigned long)g_stat_drop);
}

/* ── SLIP encode & send ──────────────────────────────────────────── */

static void slip_send(int fd, const uint8_t *pkt, size_t pktlen)
{
    if (pktlen > MAXPKT)
        pktlen = MAXPKT;

    /*
     * Fast path: if no special bytes exist, use writev to avoid copying.
     * We still clamp to MAXPKT above.
     */
    if (!memchr(pkt, SLIP_END, pktlen) && !memchr(pkt, SLIP_ESC, pktlen)) {
        struct iovec iov[3] = {
            { .iov_base = (void *)SLIP_END_S, .iov_len = 1      },
            { .iov_base = (void *)pkt,        .iov_len = pktlen  },
            { .iov_base = (void *)SLIP_END_S, .iov_len = 1      },
        };
        if (writev_all(fd, iov, 3) == -1) {
            LOG_ERR("slip_send writev: %s", strerror(errno));
            g_stat_drop++;
        }
        return;
    }

    /* Slow path: byte-stuff into temp buffer */
    uint8_t buf[SLIP_MAXPKT];
    size_t i = 0;
    buf[i++] = SLIP_END;

    for (size_t c = 0; c < pktlen; c++) {
        if (pkt[c] == SLIP_END) {
            buf[i++] = SLIP_ESC;
            buf[i++] = SLIP_ESC_END;
        } else if (pkt[c] == SLIP_ESC) {
            buf[i++] = SLIP_ESC;
            buf[i++] = SLIP_ESC_ESC;
        } else {
            buf[i++] = pkt[c];
        }
    }

    buf[i++] = SLIP_END;

    if (write_all(fd, buf, i) == -1) {
        LOG_ERR("slip_send write: %s", strerror(errno));
        g_stat_drop++;
    }
}

/* ── SLIP decode & receive ───────────────────────────────────────── */

/*
 * Non-recursive SLIP frame decoder.
 * Returns pointer to decoded IP packet (valid until next call), or NULL.
 * The returned pointer has 14 bytes of headroom for Ethernet header prepend.
 */
static uint8_t *slip_recv(int fd, slip_recv_state_t *s, size_t *pktlen)
{
retry:
    ;
    uint8_t *p;

    /* Check if we already have a complete frame buffered */
    if (s->index > s->discard &&
        (p = memchr(SLIP_BUF(s) + s->discard, SLIP_END, s->index - s->discard)))
        goto decode;

    /* Read more data until we find an END marker */
    for (;;) {
        /* Compact buffer */
        if (s->discard > 0) {
            s->index -= s->discard;
            memmove(SLIP_BUF(s), SLIP_BUF(s) + s->discard, s->index);
            s->discard = 0;
        }

        /* Buffer overflow protection: reset */
        if (s->index >= SLIP_MAXPKT) {
            LOG_WARN("slip_recv: buffer overflow, resetting");
            s->index = 0;
            g_stat_drop++;
        }

        size_t space = SLIP_MAXPKT - s->index;
        ssize_t n = read(fd, SLIP_BUF(s) + s->index, space);
        if (n > 0) {
            s->index += (size_t)n;
            p = memchr(SLIP_BUF(s), SLIP_END, s->index);
            if (p)
                goto decode;
        } else if (n == 0) {
            return NULL;  /* EOF */
        } else {
            /* n == -1 */
            if (errno == EINTR)
                continue;
            if (errno == EAGAIN || errno == EWOULDBLOCK)
                return NULL;  /* No more data right now — come back later */
            LOG_ERR("slip_recv read: %s", strerror(errno));
            return NULL;
        }
    }

decode:
    ;
    uint8_t *base = SLIP_BUF(s) + s->discard;
    *pktlen = (size_t)(p - base);
    size_t frame_consumed = *pktlen + 1; /* +1 for the END byte */

    /* Empty frame (consecutive ENDs) — skip and try again (iterative, not recursive) */
    if (*pktlen == 0) {
        s->discard += 1;
        goto retry;
    }

    /* Check for ESC bytes — if none, return zero-copy pointer */
    if (!memchr(base, SLIP_ESC, *pktlen)) {
        s->discard += frame_consumed;
        return base;
    }

    /* Unescape into scratch buffer */
    size_t out = 0;
    for (size_t c = 0; c < *pktlen && out < MAXPKT; c++) {
        if (base[c] == SLIP_ESC) {
            c++;
            if (c >= *pktlen) {
                /* Truncated ESC at end of frame — malformed, drop */
                LOG_DBG("slip_recv: truncated ESC sequence");
                g_stat_drop++;
                s->discard += frame_consumed;
                goto retry;
            }
            if (base[c] == SLIP_ESC_END)
                s->scratch[out++] = SLIP_END;
            else if (base[c] == SLIP_ESC_ESC)
                s->scratch[out++] = SLIP_ESC;
            else {
                /* Unknown ESC sequence — pass through per RFC 1055 */
                s->scratch[out++] = base[c];
            }
        } else {
            s->scratch[out++] = base[c];
        }
    }

    s->discard += frame_consumed;
    *pktlen = out;
    return s->scratch;
}

/* ── Packet bridging ─────────────────────────────────────────────── */

/*
 * SLIP frame (IP packet) → prepend Ethernet header → inject into slirp.
 * The caller guarantees 14 bytes of headroom before pkt.
 */
static void slip_to_outside(struct vdeslirp *myslirp, uint8_t *pkt, size_t pktlen)
{
    if (pktlen < 20) {
        /* Too small for any valid IP header */
        g_stat_drop++;
        return;
    }

    /* Prepend Ethernet header in-place using pre-allocated headroom */
    uint8_t *frame = pkt - 14;
    memcpy(frame,      GATEWAYMAC,    6);      /* dst MAC */
    memcpy(frame + 6,  MYMAC,         6);      /* src MAC */
    memcpy(frame + 12, "\x08\x00",    2);      /* EtherType = IPv4 */

    ssize_t sent = vdeslirp_send(myslirp, frame, pktlen + 14);
    if (sent < 0) {
        LOG_ERR("vdeslirp_send: %s", strerror(errno));
        g_stat_drop++;
        return;
    }

    g_stat_tx_packets++;
    g_stat_tx_bytes += pktlen;
    LOG_DBG("tx: %zu bytes", pktlen);
}

/*
 * Ethernet frame from slirp → strip header → SLIP encode → stdout.
 * Also handles ARP requests for the guest IP.
 */
static void outside_to_slip(struct vdeslirp *myslirp, const uint8_t *pkt, size_t pktlen)
{
    if (pktlen < 14) {
        g_stat_drop++;
        return;
    }

    uint16_t ethertype;
    memcpy(&ethertype, pkt + 12, 2);

    if (ethertype == htons(0x0806)) {
        /* ── ARP ─────────────────────────────────────────────── */
        if (pktlen < 42)
            return;

        /* Validate: Ethernet+IPv4, opcode=request, target IP=MYIP */
        if (memcmp(pkt + 14, "\x00\x01\x08\x00", 4) != 0 ||  /* hw=Ethernet, proto=IPv4 */
            memcmp(pkt + 20, "\x00\x01", 2) != 0 ||            /* opcode=request */
            memcmp(pkt + 38, MYIP, 4) != 0)                     /* target IP */
            return;

        /* Build ARP reply */
        uint8_t reply[42];
        memcpy(reply,      pkt + 22, 6);           /* dst MAC = requester's MAC */
        memcpy(reply + 6,  MYMAC, 6);              /* src MAC = our MAC */
        memcpy(reply + 12, "\x08\x06", 2);         /* EtherType = ARP */
        memcpy(reply + 14, "\x00\x01", 2);         /* hw type = Ethernet */
        memcpy(reply + 16, "\x08\x00", 2);         /* proto = IPv4 */
        reply[18] = 6;                              /* hw addr len */
        reply[19] = 4;                              /* proto addr len */
        memcpy(reply + 20, "\x00\x02", 2);         /* opcode = reply */
        memcpy(reply + 22, MYMAC, 6);               /* sender MAC */
        memcpy(reply + 28, MYIP, 4);                /* sender IP */
        memcpy(reply + 32, pkt + 22, 6);            /* target MAC */
        memcpy(reply + 38, pkt + 28, 4);            /* target IP */

        ssize_t sent = vdeslirp_send(myslirp, reply, 42);
        if (sent < 0)
            LOG_ERR("ARP reply vdeslirp_send: %s", strerror(errno));

        g_stat_arp_reply++;
        LOG_DBG("ARP reply sent");

    } else if (ethertype == htons(0x0800)) {
        /* ── IPv4 ────────────────────────────────────────────── */
        if (memcmp(pkt, MYMAC, 6) != 0) {
            g_stat_drop++;
            return; /* Not addressed to us */
        }

        slip_send(STDOUT_FILENO, pkt + 14, pktlen - 14);
        g_stat_rx_packets++;
        g_stat_rx_bytes += pktlen - 14;
        LOG_DBG("rx: %zu bytes", pktlen - 14);

    } else {
        /* Non-IPv4, non-ARP — drop (e.g. IPv6 — not supported yet) */
        g_stat_drop++;
        LOG_DBG("drop: unknown ethertype 0x%04x", ntohs(ethertype));
    }
}

/* ── Main ────────────────────────────────────────────────────────── */

static void usage(const char *prog)
{
    fprintf(stderr,
        "Usage: %s [-v] [-c config.yaml]\n"
        "\n"
        "  -v           Verbose/debug logging\n"
        "  -c FILE      Path to config file (default: slirp.yaml)\n"
        "  -h           Show this help\n"
        "\n"
        "Designed for User Mode Linux:\n"
        "  ./vmlinux eth0=slirp,,/path/to/%s\n",
        prog, prog);
}

int main(int argc, char *argv[])
{
    const char *config_path = NULL;

    int opt;
    while ((opt = getopt(argc, argv, "vhc:")) != -1) {
        switch (opt) {
        case 'v':
            g_verbose = true;
            break;
        case 'c':
            config_path = optarg;
            break;
        case 'h':
        default:
            usage(argv[0]);
            return (opt == 'h') ? 0 : 1;
        }
    }

    install_signal_handlers();

    /* ── Load YAML config ──────────────────────────────────────── */
    slirp_config_t cfg;
    if (config_path) {
        /* -c was given: use that path directly */
        if (load_yaml_config(config_path, &cfg) != 0) {
            LOG_ERR("failed to load config from %s", config_path);
            return 1;
        }
    } else {
        /* Auto-discover slirp.yaml */
        const char *exe = (argc > 0) ? argv[0] : "slirp";
        load_yaml_config(exe, &cfg);
    }

    /* ── Initialize slirp ──────────────────────────────────────── */
    SlirpConfig slirpcfg;
    vdeslirp_init(&slirpcfg, VDE_INIT_DEFAULT);
    slirpcfg.in6_enabled = false;

    struct vdeslirp *myslirp = vdeslirp_open(&slirpcfg);
    if (!myslirp) {
        LOG_ERR("vdeslirp_open failed");
        return 1;
    }

    /* ── Apply port forwards ───────────────────────────────────── */
    apply_port_forwards(myslirp, &cfg);

    /* ── Set stdin non-blocking ────────────────────────────────── */
    int flags = fcntl(STDIN_FILENO, F_GETFL, 0);
    if (flags == -1 || fcntl(STDIN_FILENO, F_SETFL, flags | O_NONBLOCK) == -1) {
        LOG_ERR("fcntl: %s", strerror(errno));
        vdeslirp_close(myslirp);
        return 1;
    }

    LOG_INFO("ready (tcp=%s udp=%s portfwd=%s verbose=%s)",
             cfg.tcp_enabled ? "on" : "off",
             cfg.udp_enabled ? "on" : "off",
             cfg.portfwd_enabled ? "on" : "off",
             g_verbose ? "on" : "off");

    /* ── Event loop ────────────────────────────────────────────── */
    struct pollfd fds[2];
    fds[0].fd     = vdeslirp_fd(myslirp);
    fds[0].events = POLLIN;
    fds[1].fd     = STDIN_FILENO;
    fds[1].events = POLLIN;

    slip_recv_state_t ss;
    memset(&ss, 0, sizeof(ss));

    while (g_running) {
        int n = poll(fds, 2, 1000);  /* 1s timeout for signal check */
        if (n == -1) {
            if (errno == EINTR)
                continue;
            LOG_ERR("poll: %s", strerror(errno));
            break;
        }
        if (n == 0)
            continue;

        /* Check for hangup */
        if ((fds[0].revents | fds[1].revents) & (POLLHUP | POLLERR))
            break;

        /* Data from slirp → SLIP encode → stdout */
        if (fds[0].revents & POLLIN) {
            uint8_t buf[MAXPKT + 14];
            ssize_t nread = vdeslirp_recv(myslirp, buf, sizeof(buf));
            if (nread > 0)
                outside_to_slip(myslirp, buf, (size_t)nread);
        }

        /* Data from stdin (SLIP) → decode → inject into slirp */
        if (fds[1].revents & POLLIN) {
            uint8_t *pkt;
            size_t pktlen;
            while ((pkt = slip_recv(STDIN_FILENO, &ss, &pktlen)) != NULL)
                slip_to_outside(myslirp, pkt, pktlen);
        }
    }

    /* ── Cleanup ───────────────────────────────────────────────── */
    print_stats();
    vdeslirp_close(myslirp);
    LOG_INFO("shutdown complete");
    return 0;
}
