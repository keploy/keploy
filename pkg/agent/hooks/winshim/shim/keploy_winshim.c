// keploy_winshim: the Windows analogue of the macOS shim
// (pkg/agent/hooks/darwin/shim/shim.c). Injected into the application under test
// with no admin and no kernel driver, it hooks Winsock's connect paths with
// MinHook and preserves keploy's source-port invariant: for each outbound TCP
// connection it makes sure the socket is bound to a known ephemeral local port,
// registers (srcPort -> real destination) with the agent over a named pipe, then
// redirects the connect to the proxy port the agent hands back. The proxy
// recovers the real destination from the source port it observes on the accepted
// connection — exactly the DestInfo.Get contract the Linux eBPF hooks and the
// macOS shim satisfy.
//
// It also moves the application's own listening ports (bind/listen), which is
// how record mode captures incoming requests. WinDivert can leave the app on its
// advertised port and redirect inbound packets in the kernel; user space has no
// such lever, so — exactly as the macOS shim does — a server bind is relocated
// to a port the agent picks, and keploy takes over the port the application
// advertises and forwards to the relocated one.
//
// Four things make this a faithful DYLD_INSERT_LIBRARIES equivalent rather than
// a demo:
//
//   * ConnectEx. Go's runtime netpoller connects through ConnectEx, which apps
//     obtain at runtime via WSAIoctl(SIO_GET_EXTENSION_FUNCTION_POINTER,
//     WSAID_CONNECTEX) rather than by importing a named symbol. Hooking connect
//     alone misses every Go program. So we hook WSAIoctl and substitute our own
//     ConnectEx wrapper for the pointer it hands back.
//
//   * Self-propagation. We are injected into the direct child of `keploy` — on
//     Windows that is `cmd /C <user command>`, which then spawns the real app.
//     A shim that stopped at cmd.exe would hook a process that makes no
//     dependency calls. So we hook CreateProcessW/A, force every child to start
//     suspended, inject ourselves into it and wait for it to arm, and only then
//     resume — recursively, for the whole process tree, exactly as dyld carries
//     an inserted library into every descendant.
//
//   * Arming off the loader lock. DllMain runs under the loader lock, where
//     LoadLibrary may deadlock — and ws2_32 is typically NOT yet loaded in a
//     freshly created suspended process, so the hooks cannot be installed there.
//     DllMain therefore only starts a thread; that thread does the real arming
//     and signals a named event when the hooks are live. Whoever injected us
//     waits for that event before resuming the process, so the application
//     cannot reach connect() before the hooks exist.
//
//   * No dependency on the environment reaching the child. A process is free to
//     hand its children a hand-built environment block, which would strip
//     KEPLOY_SHIM_PIPE and silently un-instrument everything below it. The pipe
//     name is therefore read from a sidecar file next to this DLL (written by
//     the client), with the environment variable kept only as an override. The
//     DLL's own path always travels with it, so the configuration always does
//     too.
#include <winsock2.h>
#include <ws2tcpip.h>
#include <mswsock.h>
#include <windows.h>
#include <stdio.h>
#include <stdarg.h>
#include <string.h>
#include "MinHook.h"

// ---------------------------------------------------------------------------
// Source-drift marker.
//
// The DLL is committed as a prebuilt asset (the Windows release binaries are
// cross-compiled and cannot build C), so there is an obvious failure mode:
// someone edits this file, does not re-run build.sh, and every unprivileged
// Windows run silently uses the old DLL. build.sh compiles the sha256 of THIS
// file in below, and .ci/scripts/check-windows-shim-asset.sh reads it back out
// of the binary and compares. That check needs no compiler, so it runs on the
// Linux CI runners too.
// ---------------------------------------------------------------------------

#ifndef KEPLOY_WINSHIM_SOURCE_SHA
#define KEPLOY_WINSHIM_SOURCE_SHA "unknown"
#endif

__attribute__((used)) static const char keployWinshimSourceMarker[] =
    "keploy_winshim_source_sha256=" KEPLOY_WINSHIM_SOURCE_SHA;

// The sidecar next to the DLL that carries the control-pipe name, and the
// environment override. Kept in sync with the Go side (winshim.go).
#define KEPLOY_SHIM_CONF "keploy_shim.conf"
#define ENV_SHIM_PIPE    "KEPLOY_SHIM_PIPE"
#define ENV_SHIM_DEBUG   "KEPLOY_SHIM_DEBUG"
#define ENV_SHIM_LOG     "KEPLOY_SHIM_LOG"

// How long an injector waits for a freshly injected process to finish arming.
// Bounded so a child that fails to arm delays the run instead of hanging it.
#define ARM_TIMEOUT_MS 5000

// ---------------------------------------------------------------------------
// Real function pointers and shim state
// ---------------------------------------------------------------------------

typedef int(WSAAPI *connect_t)(SOCKET, const struct sockaddr *, int);
typedef int(WSAAPI *WSAConnect_t)(SOCKET, const struct sockaddr *, int, LPWSABUF, LPWSABUF, LPQOS, LPQOS);
typedef int(WSAAPI *WSAIoctl_t)(SOCKET, DWORD, LPVOID, DWORD, LPVOID, DWORD, LPDWORD, LPWSAOVERLAPPED, LPWSAOVERLAPPED_COMPLETION_ROUTINE);
typedef int(WSAAPI *bind_t)(SOCKET, const struct sockaddr *, int);
typedef int(WSAAPI *listen_t)(SOCKET, int);
typedef int(WSAAPI *closesocket_t)(SOCKET);
typedef int(WSAAPI *getaddrinfo_t)(PCSTR, PCSTR, const ADDRINFOA *, PADDRINFOA *);
typedef INT(WSAAPI *GetAddrInfoW_t)(PCWSTR, PCWSTR, const ADDRINFOW *, PADDRINFOW *);
typedef BOOL(WINAPI *CreateProcessW_t)(LPCWSTR, LPWSTR, LPSECURITY_ATTRIBUTES, LPSECURITY_ATTRIBUTES, BOOL, DWORD, LPVOID, LPCWSTR, LPSTARTUPINFOW, LPPROCESS_INFORMATION);
typedef BOOL(WINAPI *CreateProcessA_t)(LPCSTR, LPSTR, LPSECURITY_ATTRIBUTES, LPSECURITY_ATTRIBUTES, BOOL, DWORD, LPVOID, LPCSTR, LPSTARTUPINFOA, LPPROCESS_INFORMATION);

static connect_t        real_connect        = NULL;
static WSAConnect_t     real_WSAConnect     = NULL;
static WSAIoctl_t       real_WSAIoctl       = NULL;
static LPFN_CONNECTEX   real_ConnectEx      = NULL;
static bind_t           real_bind           = NULL;
static listen_t         real_listen         = NULL;
static closesocket_t    real_closesocket    = NULL;
static getaddrinfo_t    real_getaddrinfo    = NULL;
static GetAddrInfoW_t   real_GetAddrInfoW   = NULL;
static CreateProcessW_t real_CreateProcessW = NULL;
static CreateProcessA_t real_CreateProcessA = NULL;

static char      g_pipe[256];
static char      g_selfPath[MAX_PATH]; // this DLL's own path, for child injection
static char      g_logPath[MAX_PATH];
static int       g_debug   = 0;
static int       g_enabled = 0;
static HINSTANCE g_self    = NULL;

// ---------------------------------------------------------------------------
// Logging (debug-gated, best-effort)
// ---------------------------------------------------------------------------

static void logline(const char *fmt, ...) {
    if (!g_debug) return;
    va_list ap;
    va_start(ap, fmt);
    if (g_logPath[0]) {
        FILE *f = fopen(g_logPath, "a");
        if (f) {
            vfprintf(f, fmt, ap);
            fclose(f);
            va_end(ap);
            return;
        }
    }
    // Fall back to the debugger stream so a run without a writable log path
    // still surfaces diagnostics (DebugView, a attached debugger).
    char buf[512];
    vsnprintf(buf, sizeof(buf), fmt, ap);
    OutputDebugStringA(buf);
    va_end(ap);
}

// ---------------------------------------------------------------------------
// Arm handshake
// ---------------------------------------------------------------------------

// armEventName builds the per-process event name that says "this process's hooks
// are live". The injector creates it before injecting and waits on it before
// resuming the process.
static void armEventName(DWORD pid, char *out, size_t n) {
    snprintf(out, n, "Local\\keploy-shim-armed-%lu", pid);
}

static void signalArmed(void) {
    char name[64];
    armEventName(GetCurrentProcessId(), name, sizeof(name));
    HANDLE ev = CreateEventA(NULL, TRUE, FALSE, name);
    if (ev) {
        SetEvent(ev);
        CloseHandle(ev);
    }
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

// readSidecar reads the control-pipe name from the file next to this DLL. The
// environment is not trusted to reach every descendant (a process may hand its
// children a hand-built environment block), but the DLL's path always does.
static int readSidecar(char *out, size_t n) {
    char path[MAX_PATH];
    strncpy(path, g_selfPath, sizeof(path) - 1);
    path[sizeof(path) - 1] = 0;
    char *slash = NULL;
    for (char *p = path; *p; p++) {
        if (*p == '\\' || *p == '/') slash = p;
    }
    if (!slash) return -1;
    slash[1] = 0;
    if (strlen(path) + strlen(KEPLOY_SHIM_CONF) >= sizeof(path)) return -1;
    strcat(path, KEPLOY_SHIM_CONF);

    FILE *f = fopen(path, "r");
    if (!f) return -1;
    if (!fgets(out, (int)n, f)) {
        fclose(f);
        return -1;
    }
    fclose(f);
    // Strip the trailing newline the writer leaves.
    size_t len = strlen(out);
    while (len > 0 && (out[len - 1] == '\n' || out[len - 1] == '\r')) out[--len] = 0;
    return out[0] ? 0 : -1;
}

// ---------------------------------------------------------------------------
// Control-pipe RPC
// ---------------------------------------------------------------------------

// askAgent sends one request line to the control pipe and reads one reply line.
// Best-effort and bounded: a wedged agent must never hang the app's connect().
static int askAgent(const char *req, char *reply, int replyLen) {
    HANDLE h = CreateFileA(g_pipe, GENERIC_READ | GENERIC_WRITE, 0, NULL, OPEN_EXISTING, 0, NULL);
    if (h == INVALID_HANDLE_VALUE) {
        // Every pipe instance is busy, or the agent is still starting. Wait
        // briefly for one to free up.
        if (WaitNamedPipeA(g_pipe, 2000)) {
            h = CreateFileA(g_pipe, GENERIC_READ | GENERIC_WRITE, 0, NULL, OPEN_EXISTING, 0, NULL);
        }
    }
    if (h == INVALID_HANDLE_VALUE) return -1;

    DWORD n = 0;
    int ok = -1;
    if (WriteFile(h, req, (DWORD)strlen(req), &n, NULL)) {
        DWORD rd = 0;
        if (ReadFile(h, reply, replyLen - 1, &rd, NULL)) {
            reply[rd] = 0;
            ok = 0;
        }
    }
    CloseHandle(h);
    return ok;
}

// ---------------------------------------------------------------------------
// Redirection core
// ---------------------------------------------------------------------------

// isV4Mapped reports whether an IPv6 address is an IPv4-mapped one
// (::ffff:a.b.c.d), which is how a dual-stack socket reaches an IPv4 host.
static int isV4Mapped(const struct in6_addr *a) {
    const unsigned char *b = (const unsigned char *)a;
    for (int i = 0; i < 10; i++) {
        if (b[i] != 0) return 0;
    }
    return b[10] == 0xff && b[11] == 0xff;
}

// ensureBoundSrcPort returns the socket's local port, binding an ephemeral port
// first if the socket is not yet bound. ConnectEx requires a pre-bound socket,
// so by the time we see one the port is already assigned; a plain connect()
// usually arrives unbound, so we bind it ourselves. Returns 0 on failure.
//
// The bind is to the wildcard address, not loopback: the socket may be on its
// way to a real remote host (a destination the agent declines to intercept still
// connects for real), and a loopback-bound socket could not reach it. It must
// also be of the socket's OWN family — binding a sockaddr_in to an AF_INET6
// socket fails, which would silently disable interception for every IPv6
// connection.
static unsigned short ensureBoundSrcPort(SOCKET s, int family) {
    struct sockaddr_storage bound;
    int blen = sizeof(bound);
    if (getsockname(s, (struct sockaddr *)&bound, &blen) == 0) {
        unsigned short port = (family == AF_INET6)
                                  ? ntohs(((struct sockaddr_in6 *)&bound)->sin6_port)
                                  : ntohs(((struct sockaddr_in *)&bound)->sin_port);
        if (port != 0) return port;
    }

    struct sockaddr_storage local;
    ZeroMemory(&local, sizeof(local));
    int localLen;
    if (family == AF_INET6) {
        struct sockaddr_in6 *a = (struct sockaddr_in6 *)&local;
        a->sin6_family = AF_INET6;
        a->sin6_addr = in6addr_any;
        localLen = (int)sizeof(*a);
    } else {
        struct sockaddr_in *a = (struct sockaddr_in *)&local;
        a->sin_family = AF_INET;
        a->sin_addr.s_addr = htonl(INADDR_ANY);
        localLen = (int)sizeof(*a);
    }
    if (bind(s, (struct sockaddr *)&local, localLen) != 0) {
        logline("could not pin a source port: bind failed %d\n", WSAGetLastError());
        return 0;
    }
    blen = sizeof(bound);
    if (getsockname(s, (struct sockaddr *)&bound, &blen) != 0) return 0;
    return (family == AF_INET6) ? ntohs(((struct sockaddr_in6 *)&bound)->sin6_port)
                                : ntohs(((struct sockaddr_in *)&bound)->sin_port);
}

// proxyAddr fills out with the loopback address of the proxy, in the form the
// socket's family can actually reach. Connecting an AF_INET6 socket to an
// AF_INET address fails outright, so getting this wrong does not merely miss a
// recording — it breaks the application's connection.
//
// An IPv4-mapped destination is retargeted at ::ffff:127.0.0.1 rather than ::1:
// the packets that leave are ordinary IPv4 packets to 127.0.0.1, so this works
// against a proxy that only listens on IPv4 loopback, which ::1 would not.
static void proxyAddr(struct sockaddr_storage *out, int *outLen, int family, int mapped,
                      unsigned short port) {
    ZeroMemory(out, sizeof(*out));
    if (family == AF_INET) {
        struct sockaddr_in *sin = (struct sockaddr_in *)out;
        sin->sin_family = AF_INET;
        sin->sin_port = htons(port);
        sin->sin_addr.s_addr = htonl(INADDR_LOOPBACK);
        *outLen = (int)sizeof(*sin);
        return;
    }
    struct sockaddr_in6 *sin6 = (struct sockaddr_in6 *)out;
    sin6->sin6_family = AF_INET6;
    sin6->sin6_port = htons(port);
    if (mapped) {
        unsigned char *b = (unsigned char *)&sin6->sin6_addr;
        b[10] = 0xff;
        b[11] = 0xff;
        b[12] = 127;
        b[15] = 1;
    } else {
        sin6->sin6_addr = in6addr_loopback;
    }
    *outLen = (int)sizeof(*sin6);
}

// redirect decides whether to steer a connection to the proxy. On success it
// fills *out with the loopback proxy address to dial instead of the app's
// destination and returns 1; it returns 0 to leave the connection alone.
static int redirect(SOCKET s, const struct sockaddr *name, int namelen,
                    struct sockaddr_storage *out, int *outLen) {
    if (!g_enabled || !name) return 0;

    unsigned short realPort;
    char realIp[64] = {0};
    int ipver, family = name->sa_family, mapped = 0;
    // Loopback is deliberately NOT filtered here. Two reasons: a dependency the
    // application reaches on localhost (a local database, a sidecar) is traffic
    // Keploy should record like any other, and — critically — the synthetic
    // addresses handed out for names that no longer resolve are themselves in
    // 127.0.0.0/8. Skipping loopback would make every mocked-by-DNS dependency
    // connect to nothing. The agent is the one that decides what to leave alone;
    // it knows its own proxy, agent and DNS ports, and this shim does not.
    if (family == AF_INET) {
        if (namelen < (int)sizeof(struct sockaddr_in)) return 0;
        const struct sockaddr_in *a = (const struct sockaddr_in *)name;
        inet_ntop(AF_INET, (void *)&a->sin_addr, realIp, sizeof(realIp));
        realPort = ntohs(a->sin_port);
        ipver = 4;
    } else if (family == AF_INET6) {
        if (namelen < (int)sizeof(struct sockaddr_in6)) return 0;
        const struct sockaddr_in6 *a = (const struct sockaddr_in6 *)name;
        mapped = isV4Mapped(&a->sin6_addr);
        inet_ntop(AF_INET6, (void *)&a->sin6_addr, realIp, sizeof(realIp));
        realPort = ntohs(a->sin6_port);
        ipver = 6;
    } else {
        return 0;
    }

    unsigned short srcPort = ensureBoundSrcPort(s, family);
    if (srcPort == 0) return 0;

    char req[256], reply[64];
    snprintf(req, sizeof(req), "CONNECT %u %d %s %u\n", srcPort, ipver, realIp, realPort);
    if (askAgent(req, reply, sizeof(reply)) != 0 || strncmp(reply, "OK", 2) != 0) {
        logline("agent declined srcPort=%u -> %s:%u (%s)\n", srcPort, realIp, realPort, reply);
        return 0; // agent said bypass, or is unreachable: leave the connection alone
    }
    // The reply is "OK <proxyPort>". The agent allocates the proxy port after
    // the client has already staged the shim, so the port is learned here rather
    // than from a variable.
    unsigned int proxyPort = 0;
    if (sscanf(reply, "OK %u", &proxyPort) != 1 || proxyPort == 0 || proxyPort > 65535) {
        logline("agent replied OK without a usable proxy port (%s)\n", reply);
        return 0;
    }
    logline("redirect srcPort=%u -> %s:%u via proxy %u\n", srcPort, realIp, realPort, proxyPort);
    proxyAddr(out, outLen, family, mapped, (unsigned short)proxyPort);
    return 1;
}

// ---------------------------------------------------------------------------
// Name resolution
//
// During replay a recorded dependency may no longer exist, or the machine may
// be offline. The application then fails inside the resolver, before it ever
// reaches connect(), so the mock that would have answered is never consulted.
// Asking the agent for a synthetic address keeps the application moving to the
// connect() the shim can actually intercept.
//
// Only the real lookup's FAILURE is substituted — a name that resolves is left
// entirely alone — which is why this is safe to leave armed during recording
// too: there, a resolution failure is a real failure and the agent declines.
//
// The substitution re-runs the real resolver with the synthetic address as a
// numeric host rather than hand-building an addrinfo chain. That matters: the
// application frees the result with freeaddrinfo/FreeAddrInfoW, which can only
// be handed memory the system itself allocated.
// ---------------------------------------------------------------------------

// askDNS returns 0 and fills ip when the agent supplies a synthetic address.
static int askDNS(const char *host, char *ip, int ipLen) {
    if (!g_enabled || !host || !host[0]) return -1;
    char req[320], reply[128];
    snprintf(req, sizeof(req), "DNS %s\n", host);
    if (askAgent(req, reply, sizeof(reply)) != 0) return -1;
    if (sscanf(reply, "IP %45s", ip) != 1) return -1;
    (void)ipLen;
    return 0;
}

static int WSAAPI hook_getaddrinfo(PCSTR node, PCSTR service, const ADDRINFOA *hints, PADDRINFOA *res) {
    int rc = real_getaddrinfo(node, service, hints, res);
    if (rc == 0 || !g_enabled || !node || !node[0]) return rc;

    char ip[64] = {0};
    if (askDNS(node, ip, sizeof(ip)) != 0) return rc;

    ADDRINFOA numeric;
    if (hints) {
        memcpy(&numeric, hints, sizeof(numeric));
    } else {
        ZeroMemory(&numeric, sizeof(numeric));
        numeric.ai_socktype = SOCK_STREAM;
    }
    numeric.ai_flags |= AI_NUMERICHOST;
    // The synthetic address is always IPv4 loopback, so drop any AF_INET6 or
    // AF_UNSPEC preference that would make the numeric parse fail.
    numeric.ai_family = AF_INET;

    if (real_getaddrinfo(ip, service, &numeric, res) != 0) return rc;
    logline("resolved %s -> %s via agent (real lookup failed: %d)\n", node, ip, rc);
    return 0;
}

static INT WSAAPI hook_GetAddrInfoW(PCWSTR node, PCWSTR service, const ADDRINFOW *hints, PADDRINFOW *res) {
    INT rc = real_GetAddrInfoW(node, service, hints, res);
    if (rc == 0 || !g_enabled || !node || !node[0]) return rc;

    char host[256] = {0};
    if (WideCharToMultiByte(CP_UTF8, 0, node, -1, host, sizeof(host) - 1, NULL, NULL) == 0) return rc;

    char ip[64] = {0};
    if (askDNS(host, ip, sizeof(ip)) != 0) return rc;

    WCHAR wideIP[64] = {0};
    if (MultiByteToWideChar(CP_UTF8, 0, ip, -1, wideIP, 63) == 0) return rc;

    ADDRINFOW numeric;
    if (hints) {
        memcpy(&numeric, hints, sizeof(numeric));
    } else {
        ZeroMemory(&numeric, sizeof(numeric));
        numeric.ai_socktype = SOCK_STREAM;
    }
    numeric.ai_flags |= AI_NUMERICHOST;
    numeric.ai_family = AF_INET;

    if (real_GetAddrInfoW(wideIP, service, &numeric, res) != 0) return rc;
    logline("resolved %s -> %s via agent (real lookup failed: %d)\n", host, ip, rc);
    return 0;
}

// ---------------------------------------------------------------------------
// Moved-bind bookkeeping
//
// A bind that was relocated has to be remembered until listen() proves the
// socket really is a server: a client is equally free to bind an explicit source
// port, and publishing an ingress event for that would make keploy stand up a
// forwarder for a port nothing serves.
//
// A small fixed table keyed by SOCKET, guarded by a critical section held only
// across the few instructions below. A server process has a handful of listening
// sockets, so this never fills; if it somehow did, the bind is simply left where
// the application asked for it.
// ---------------------------------------------------------------------------

#define MOVED_SLOTS 64

typedef struct {
    SOCKET         sock;
    unsigned short origPort;
    unsigned short movedPort;
    int            listened;
    int            used;
} movedslot_t;

static movedslot_t     g_moved[MOVED_SLOTS];
static CRITICAL_SECTION g_movedLock;
static int              g_movedLockReady = 0;

// movedReserve claims a slot BEFORE the bind is moved, returning its index or
// -1 when the table is full.
//
// Reserving first is what makes a full table harmless. Moving the listener and
// only then discovering there is nowhere to record it would be the worst
// outcome available: the application ends up on the relocated port, the agent
// never hears the LISTEN that would make it take over the advertised port, and
// the application is unreachable on the address it advertises. Failing to
// reserve simply leaves the application where it asked to be.
static int movedReserve(SOCKET s) {
    if (!g_movedLockReady) return -1;
    int idx = -1;
    EnterCriticalSection(&g_movedLock);
    for (int i = 0; i < MOVED_SLOTS; i++) {
        if (!g_moved[i].used) {
            g_moved[i].sock = s;
            g_moved[i].origPort = 0;
            g_moved[i].movedPort = 0;
            g_moved[i].listened = 0;
            g_moved[i].used = 1;
            idx = i;
            break;
        }
    }
    LeaveCriticalSection(&g_movedLock);
    return idx;
}

static void movedRelease(int idx) {
    if (idx < 0 || !g_movedLockReady) return;
    EnterCriticalSection(&g_movedLock);
    g_moved[idx].used = 0;
    LeaveCriticalSection(&g_movedLock);
}

static void movedCommit(int idx, unsigned short origPort, unsigned short movedPort) {
    if (idx < 0 || !g_movedLockReady) return;
    EnterCriticalSection(&g_movedLock);
    g_moved[idx].origPort = origPort;
    g_moved[idx].movedPort = movedPort;
    LeaveCriticalSection(&g_movedLock);
}

// movedClaimForListen returns 1 exactly once per moved socket, the first time
// listen() succeeds on it, handing back the port pair to report.
static int movedClaimForListen(SOCKET s, unsigned short *origPort, unsigned short *movedPort) {
    int claimed = 0;
    if (!g_movedLockReady) return 0;
    EnterCriticalSection(&g_movedLock);
    for (int i = 0; i < MOVED_SLOTS; i++) {
        if (g_moved[i].used && g_moved[i].sock == s && !g_moved[i].listened &&
            g_moved[i].movedPort != 0) {
            g_moved[i].listened = 1;
            *origPort = g_moved[i].origPort;
            *movedPort = g_moved[i].movedPort;
            claimed = 1;
            break;
        }
    }
    LeaveCriticalSection(&g_movedLock);
    return claimed;
}

static void movedForget(SOCKET s) {
    if (!g_movedLockReady) return;
    EnterCriticalSection(&g_movedLock);
    for (int i = 0; i < MOVED_SLOTS; i++) {
        if (g_moved[i].used && g_moved[i].sock == s) {
            g_moved[i].used = 0;
            break;
        }
    }
    LeaveCriticalSection(&g_movedLock);
}

// ---------------------------------------------------------------------------
// bind / listen hooks — ingress
// ---------------------------------------------------------------------------

static int WSAAPI hook_bind(SOCKET s, const struct sockaddr *name, int namelen) {
    if (!g_enabled || !name) return real_bind(s, name, namelen);

    int family = name->sa_family;
    int want;
    if (family == AF_INET) {
        want = (int)sizeof(struct sockaddr_in);
    } else if (family == AF_INET6) {
        want = (int)sizeof(struct sockaddr_in6);
    } else {
        return real_bind(s, name, namelen);
    }
    // A caller whose namelen is too short for the family is handed straight to
    // Winsock so it sees the native result, rather than having the shim turn an
    // invalid bind into a successful one on a relocated port.
    if (namelen < want || namelen > (int)sizeof(struct sockaddr_storage)) {
        return real_bind(s, name, namelen);
    }

    int sockType = 0, typeLen = sizeof(sockType);
    if (getsockopt(s, SOL_SOCKET, SO_TYPE, (char *)&sockType, &typeLen) != 0 || sockType != SOCK_STREAM) {
        return real_bind(s, name, namelen);
    }

    unsigned short origPort = (family == AF_INET)
                                  ? ntohs(((const struct sockaddr_in *)name)->sin_port)
                                  : ntohs(((const struct sockaddr_in6 *)name)->sin6_port);
    // Port 0 is an ephemeral client bind (including the one ensureBoundSrcPort
    // does), never a server advertising an address. Leave it alone.
    if (origPort == 0) return real_bind(s, name, namelen);

    // Claim somewhere to record the move BEFORE making it, so a full table can
    // only mean "no move", never "moved but unreportable".
    int slot = movedReserve(s);
    if (slot < 0) {
        logline("no room to track a moved bind; leaving the listener on %u\n", origPort);
        return real_bind(s, name, namelen);
    }

    char req[128], reply[128];
    snprintf(req, sizeof(req), "BIND %lu %u\n", GetCurrentProcessId(), origPort);
    if (askAgent(req, reply, sizeof(reply)) != 0) {
        movedRelease(slot);
        return real_bind(s, name, namelen);
    }
    unsigned int newPort = 0;
    if (sscanf(reply, "PORT %u", &newPort) != 1 || newPort == 0 || newPort > 65535) {
        // "KEEP", or anything unexpected: bind where the application asked.
        movedRelease(slot);
        return real_bind(s, name, namelen);
    }

    // Rewrite onto a zeroed copy of exactly the bytes the caller supplied; the
    // caller's sockaddr is const and may be reused. Anything the caller did not
    // provide stays zero rather than whatever was on the stack, and the caller's
    // own namelen is what reaches Winsock.
    struct sockaddr_storage moved;
    ZeroMemory(&moved, sizeof(moved));
    memcpy(&moved, name, (size_t)namelen);
    if (family == AF_INET) {
        ((struct sockaddr_in *)&moved)->sin_port = htons((unsigned short)newPort);
    } else {
        ((struct sockaddr_in6 *)&moved)->sin6_port = htons((unsigned short)newPort);
    }

    if (real_bind(s, (struct sockaddr *)&moved, namelen) == 0) {
        movedCommit(slot, origPort, (unsigned short)newPort);
        logline("moved bind %u -> %u so keploy can own %u\n", origPort, newPort, origPort);
        return 0;
    }
    movedRelease(slot);

    // The agent picked this port by binding :0 and closing it, so anything on
    // the machine can take it in the window before the app binds. Returning that
    // failure verbatim would hand the application an error for a port it never
    // asked for and which was actually free — keploy breaking an app that would
    // otherwise have started. Fall back to what the app requested: the run then
    // records no ingress for this port, which is a far better outcome than not
    // running at all.
    logline("could not move the listener %u -> %u (%d); leaving it on %u\n",
            origPort, newPort, WSAGetLastError(), origPort);
    return real_bind(s, name, namelen);
}

// hook_listen reports a moved bind that turned out to be a real server socket.
// Only then does the agent know to take over the application's advertised port
// and forward to the relocated one.
static int WSAAPI hook_listen(SOCKET s, int backlog) {
    int rc = real_listen(s, backlog);
    if (rc != 0 || !g_enabled) return rc;

    int saved = WSAGetLastError();
    unsigned short origPort = 0, movedPort = 0;
    if (movedClaimForListen(s, &origPort, &movedPort)) {
        char req[128], reply[64];
        snprintf(req, sizeof(req), "LISTEN %lu %u %u\n", GetCurrentProcessId(), origPort, movedPort);
        if (askAgent(req, reply, sizeof(reply)) != 0) {
            logline("LISTEN %u->%u went unanswered\n", origPort, movedPort);
        } else {
            logline("confirmed server socket: listen on moved bind %u -> %u\n", origPort, movedPort);
        }
    }
    WSASetLastError(saved);
    return rc;
}

static int WSAAPI hook_closesocket(SOCKET s) {
    movedForget(s);
    return real_closesocket(s);
}

// ---------------------------------------------------------------------------
// Winsock connect hooks
// ---------------------------------------------------------------------------

static int WSAAPI hook_connect(SOCKET s, const struct sockaddr *name, int namelen) {
    struct sockaddr_storage proxy;
    int proxyLen = 0;
    if (redirect(s, name, namelen, &proxy, &proxyLen)) {
        return real_connect(s, (struct sockaddr *)&proxy, proxyLen);
    }
    return real_connect(s, name, namelen);
}

static int WSAAPI hook_WSAConnect(SOCKET s, const struct sockaddr *name, int namelen,
                                  LPWSABUF cd, LPWSABUF caller, LPQOS sqos, LPQOS gqos) {
    struct sockaddr_storage proxy;
    int proxyLen = 0;
    if (redirect(s, name, namelen, &proxy, &proxyLen)) {
        return real_WSAConnect(s, (struct sockaddr *)&proxy, proxyLen, cd, caller, sqos, gqos);
    }
    return real_WSAConnect(s, name, namelen, cd, caller, sqos, gqos);
}

// hook_ConnectEx is the substitute handed back from WSAIoctl. Go, and any other
// overlapped-I/O client, connects through this pointer.
static BOOL PASCAL hook_ConnectEx(SOCKET s, const struct sockaddr *name, int namelen,
                                  PVOID lpSendBuffer, DWORD dwSendDataLength,
                                  LPDWORD lpdwBytesSent, LPOVERLAPPED lpOverlapped) {
    struct sockaddr_storage proxy;
    int proxyLen = 0;
    if (redirect(s, name, namelen, &proxy, &proxyLen)) {
        return real_ConnectEx(s, (struct sockaddr *)&proxy, proxyLen,
                              lpSendBuffer, dwSendDataLength, lpdwBytesSent, lpOverlapped);
    }
    return real_ConnectEx(s, name, namelen, lpSendBuffer, dwSendDataLength, lpdwBytesSent, lpOverlapped);
}

// hook_WSAIoctl intercepts the one request that matters — an application asking
// for the ConnectEx function pointer — and substitutes our wrapper, capturing
// the real pointer so the wrapper can forward to it. Every other ioctl passes
// straight through.
static int WSAAPI hook_WSAIoctl(SOCKET s, DWORD code, LPVOID inBuf, DWORD inLen,
                                LPVOID outBuf, DWORD outLen, LPDWORD bytesRet,
                                LPWSAOVERLAPPED ov, LPWSAOVERLAPPED_COMPLETION_ROUTINE cr) {
    int rc = real_WSAIoctl(s, code, inBuf, inLen, outBuf, outLen, bytesRet, ov, cr);
    if (rc == 0 && code == SIO_GET_EXTENSION_FUNCTION_POINTER && inBuf && inLen >= sizeof(GUID) &&
        outBuf && outLen >= sizeof(void *)) {
        GUID connectExGuid = WSAID_CONNECTEX;
        if (memcmp(inBuf, &connectExGuid, sizeof(GUID)) == 0) {
            LPFN_CONNECTEX *slot = (LPFN_CONNECTEX *)outBuf;
            if (*slot && *slot != hook_ConnectEx) {
                real_ConnectEx = *slot;
                *slot = hook_ConnectEx;
                logline("substituted the ConnectEx pointer (pid=%lu)\n", GetCurrentProcessId());
            }
        }
    }
    return rc;
}

// ---------------------------------------------------------------------------
// Self-propagation into child processes
// ---------------------------------------------------------------------------

// injectSelfInto loads this DLL into an existing (suspended) process by the same
// LoadLibrary-in-a-remote-thread technique the client uses for the first
// process, then waits for that process to signal that its hooks are live.
// Best-effort: a failure leaves the child running uninstrumented, never broken.
static void injectSelfInto(HANDLE proc, DWORD pid) {
    if (!g_selfPath[0]) return;

    // Create the arm event BEFORE injecting, so the child cannot signal into a
    // void and we cannot miss it.
    char evName[64];
    armEventName(pid, evName, sizeof(evName));
    HANDLE armed = CreateEventA(NULL, TRUE, FALSE, evName);

    SIZE_T len = strlen(g_selfPath) + 1;
    LPVOID remote = VirtualAllocEx(proc, NULL, len, MEM_COMMIT | MEM_RESERVE, PAGE_READWRITE);
    if (!remote) {
        logline("child VirtualAllocEx failed %lu\n", GetLastError());
        if (armed) CloseHandle(armed);
        return;
    }
    if (!WriteProcessMemory(proc, remote, g_selfPath, len, NULL)) {
        logline("child WriteProcessMemory failed %lu\n", GetLastError());
        VirtualFreeEx(proc, remote, 0, MEM_RELEASE);
        if (armed) CloseHandle(armed);
        return;
    }
    HMODULE k32 = GetModuleHandleA("kernel32.dll");
    FARPROC loadLib = k32 ? GetProcAddress(k32, "LoadLibraryA") : NULL;
    if (!loadLib) {
        VirtualFreeEx(proc, remote, 0, MEM_RELEASE);
        if (armed) CloseHandle(armed);
        return;
    }
    // LoadLibraryA's signature is not LPTHREAD_START_ROUTINE's, but the two are
    // ABI-compatible here (one pointer argument, pointer-sized return) and this
    // is the standard injection idiom. The cast goes through void * so the
    // compiler does not reject the deliberate function-type mismatch.
    HANDLE th = CreateRemoteThread(proc, NULL, 0, (LPTHREAD_START_ROUTINE)(void *)loadLib, remote, 0, NULL);
    if (!th) {
        // The usual cause is an architecture mismatch (a 32-bit child of a
        // 64-bit parent). The child simply runs uninstrumented.
        logline("child CreateRemoteThread failed %lu (pid=%lu)\n", GetLastError(), pid);
        VirtualFreeEx(proc, remote, 0, MEM_RELEASE);
        if (armed) CloseHandle(armed);
        return;
    }
    WaitForSingleObject(th, ARM_TIMEOUT_MS);
    CloseHandle(th);

    if (armed) {
        if (WaitForSingleObject(armed, ARM_TIMEOUT_MS) != WAIT_OBJECT_0) {
            logline("child pid=%lu did not arm within the timeout\n", pid);
        }
        CloseHandle(armed);
    }
    VirtualFreeEx(proc, remote, 0, MEM_RELEASE);
    logline("propagated the shim into child pid=%lu\n", pid);
}

// propagate runs the common tail of both CreateProcess hooks: the child was
// forced to start suspended, so inject the shim, then resume it — unless the
// caller asked for a suspended child, whose contract must be preserved.
static BOOL propagate(BOOL created, DWORD callerFlags, LPPROCESS_INFORMATION pi) {
    if (!created || !pi) return created;
    injectSelfInto(pi->hProcess, pi->dwProcessId);
    if (!(callerFlags & CREATE_SUSPENDED)) {
        ResumeThread(pi->hThread);
    }
    return created;
}

static BOOL WINAPI hook_CreateProcessW(LPCWSTR app, LPWSTR cmd, LPSECURITY_ATTRIBUTES pa,
                                       LPSECURITY_ATTRIBUTES ta, BOOL inherit, DWORD flags,
                                       LPVOID env, LPCWSTR dir, LPSTARTUPINFOW si,
                                       LPPROCESS_INFORMATION pi) {
    BOOL ok = real_CreateProcessW(app, cmd, pa, ta, inherit, flags | CREATE_SUSPENDED, env, dir, si, pi);
    return propagate(ok, flags, pi);
}

static BOOL WINAPI hook_CreateProcessA(LPCSTR app, LPSTR cmd, LPSECURITY_ATTRIBUTES pa,
                                       LPSECURITY_ATTRIBUTES ta, BOOL inherit, DWORD flags,
                                       LPVOID env, LPCSTR dir, LPSTARTUPINFOA si,
                                       LPPROCESS_INFORMATION pi) {
    BOOL ok = real_CreateProcessA(app, cmd, pa, ta, inherit, flags | CREATE_SUSPENDED, env, dir, si, pi);
    return propagate(ok, flags, pi);
}

// ---------------------------------------------------------------------------
// Arming
// ---------------------------------------------------------------------------

static void createHook(const char *mod, const char *fn, void *detour, void **orig) {
    HMODULE m = GetModuleHandleA(mod);
    if (!m) m = LoadLibraryA(mod);
    if (!m) {
        logline("module %s unavailable; %s not hooked\n", mod, fn);
        return;
    }
    void *target = (void *)GetProcAddress(m, fn);
    if (target && MH_CreateHook(target, detour, orig) == MH_OK && MH_EnableHook(target) == MH_OK) {
        return;
    }
    logline("failed to hook %s!%s\n", mod, fn);
}

// sayHello announces this process to the agent. The first HELLO is the proof
// that instrumentation actually reached the application: without it a run is
// indistinguishable from an app that made no dependency calls.
static void sayHello(void) {
    char prog[MAX_PATH] = {0};
    GetModuleFileNameA(NULL, prog, sizeof(prog) - 1);
    const char *base = prog;
    for (const char *p = prog; *p; p++) {
        if (*p == '\\' || *p == '/') base = p + 1;
    }
    char req[MAX_PATH + 32], reply[64];
    snprintf(req, sizeof(req), "HELLO %lu %s\n", GetCurrentProcessId(), base);
    askAgent(req, reply, sizeof(reply));
}

// armThread does the real work, off the loader lock. Started by DllMain, which
// must not do any of this itself: LoadLibrary under the loader lock can
// deadlock, and ws2_32 is usually not yet loaded in a freshly created suspended
// process.
static DWORD WINAPI armThread(LPVOID unused) {
    (void)unused;

    const char *dbg = getenv(ENV_SHIM_DEBUG);
    g_debug = dbg && dbg[0] == '1';
    const char *logPath = getenv(ENV_SHIM_LOG);
    if (logPath && logPath[0]) {
        strncpy(g_logPath, logPath, sizeof(g_logPath) - 1);
    }

    // The environment is an override; the sidecar next to this DLL is the
    // source of truth, because it travels with the DLL into every descendant
    // regardless of what environment block a parent hands its children.
    const char *envPipe = getenv(ENV_SHIM_PIPE);
    if (envPipe && envPipe[0]) {
        strncpy(g_pipe, envPipe, sizeof(g_pipe) - 1);
    } else if (readSidecar(g_pipe, sizeof(g_pipe)) != 0) {
        logline("no control pipe configured; not arming (pid=%lu)\n", GetCurrentProcessId());
        signalArmed(); // never leave an injector waiting on a shim that gave up
        return 0;
    }

    if (MH_Initialize() != MH_OK) {
        logline("MH_Initialize failed\n");
        signalArmed();
        return 0;
    }

    InitializeCriticalSection(&g_movedLock);
    g_movedLockReady = 1;

    createHook("ws2_32.dll", "connect", (void *)hook_connect, (void **)&real_connect);
    createHook("ws2_32.dll", "WSAConnect", (void *)hook_WSAConnect, (void **)&real_WSAConnect);
    createHook("ws2_32.dll", "WSAIoctl", (void *)hook_WSAIoctl, (void **)&real_WSAIoctl);
    createHook("ws2_32.dll", "bind", (void *)hook_bind, (void **)&real_bind);
    createHook("ws2_32.dll", "listen", (void *)hook_listen, (void **)&real_listen);
    createHook("ws2_32.dll", "closesocket", (void *)hook_closesocket, (void **)&real_closesocket);
    createHook("ws2_32.dll", "getaddrinfo", (void *)hook_getaddrinfo, (void **)&real_getaddrinfo);
    createHook("ws2_32.dll", "GetAddrInfoW", (void *)hook_GetAddrInfoW, (void **)&real_GetAddrInfoW);
    createHook("kernel32.dll", "CreateProcessW", (void *)hook_CreateProcessW, (void **)&real_CreateProcessW);
    createHook("kernel32.dll", "CreateProcessA", (void *)hook_CreateProcessA, (void **)&real_CreateProcessA);

    g_enabled = 1;
    logline("armed: pipe=%s pid=%lu dll=%s\n", g_pipe, GetCurrentProcessId(), g_selfPath);

    sayHello();

    // Only now may the process be resumed.
    signalArmed();
    return 0;
}

BOOL WINAPI DllMain(HINSTANCE h, DWORD reason, LPVOID r) {
    (void)r;
    if (reason == DLL_PROCESS_ATTACH) {
        g_self = h;
        DisableThreadLibraryCalls(h);
        // Resolve our own path here: it is needed by armThread and is cheap and
        // safe to read under the loader lock.
        GetModuleFileNameA(h, g_selfPath, sizeof(g_selfPath) - 1);
        HANDLE t = CreateThread(NULL, 0, armThread, NULL, 0, NULL);
        if (t) {
            CloseHandle(t);
        }
    }
    return TRUE;
}
