// keploy-cbshim — LD_PRELOAD shim that fixes SCRAM channel binding under a
// TLS-MITM proxy.
//
// Idea: when libpq calls OpenSSL's X509_digest() to hash the peer cert (the
// MITM proxy's cert), we look up the hash of the REAL upstream cert (which
// the MITM proxy has already written into a known file) and overwrite the
// output buffer with that. libpq then builds a SCRAM proof using the real
// cert's hash, the proof verifies on the server, and auth succeeds.
//
// Build:
//   gcc -O0 -g -Wall -Wextra -fPIC -shared \
//       -o cbshim.so cbshim.c -ldl -lcrypto
//
// Use:
//   HASHMAP=/tmp/scram-poc-hashmap LD_PRELOAD=./cbshim.so ./client ...
//
// Hashmap file format (one entry per line, two hex strings separated by space):
//   <H(mitm_cert)_hex>  <H(real_cert)_hex>
//
// The shim re-reads the file lazily and caches entries. It is process-wide
// (no per-thread state). Lookups are O(N) over a small table — N is the
// number of distinct upstream Postgres hosts, never per-connection.

#define _GNU_SOURCE
#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <pthread.h>
#include <openssl/evp.h>
#include <openssl/x509.h>

// ---- logging ---------------------------------------------------------------
//
// Default: silent. Set CBSHIM_DEBUG=1 to see swap activity on stderr.

static int debug_enabled(void) {
    static int v = -1;
    if (v == -1) {
        const char *s = getenv("CBSHIM_DEBUG");
        v = (s && *s && *s != '0') ? 1 : 0;
    }
    return v;
}

#define DLOG(fmt, ...) do { \
    if (debug_enabled()) fprintf(stderr, "[cbshim] " fmt "\n", ##__VA_ARGS__); \
} while (0)

// ---- hash table -----------------------------------------------------------

#define MAX_ENTRIES 64
#define MAX_HASH_LEN 64   // SHA-512 is the largest channel-binding hash size

struct entry {
    unsigned char mitm_hash[MAX_HASH_LEN];
    unsigned int  mitm_len;
    unsigned char real_hash[MAX_HASH_LEN];
    unsigned int  real_len;
};

static struct entry table[MAX_ENTRIES];
static int table_n = 0;
static pthread_mutex_t table_mu = PTHREAD_MUTEX_INITIALIZER;

// hex2bin: decode a hex string into bytes. Returns the number of bytes decoded,
// or -1 on a malformed character. `out` must be at least len/2 bytes.
static int hex2bin(const char *hex, size_t len, unsigned char *out) {
    if (len % 2 != 0) return -1;
    for (size_t i = 0; i < len; i += 2) {
        int hi = -1, lo = -1;
        char a = hex[i], b = hex[i + 1];
        hi = (a >= '0' && a <= '9') ? a - '0' :
             (a >= 'a' && a <= 'f') ? a - 'a' + 10 :
             (a >= 'A' && a <= 'F') ? a - 'A' + 10 : -1;
        lo = (b >= '0' && b <= '9') ? b - '0' :
             (b >= 'a' && b <= 'f') ? b - 'a' + 10 :
             (b >= 'A' && b <= 'F') ? b - 'A' + 10 : -1;
        if (hi < 0 || lo < 0) return -1;
        out[i / 2] = (unsigned char)((hi << 4) | lo);
    }
    return (int)(len / 2);
}

// load_table_locked re-reads the hashmap file into the table. Called under
// table_mu. We always reload from scratch so live edits show up; for our
// PoC scale (N small, called once per X509_digest invocation worst case)
// this is plenty fast.
static void load_table_locked(void) {
    // Default matches keploy's cbmap.DefaultPath. The CBSHIM_HASHMAP
    // env var overrides — keploy sets it on the app process to
    // whatever cbmap.Path() resolves to (including the
    // KEPLOY_CBMAP_PATH-overridden value), so the shim and keploy
    // always agree on the file.
    const char *path = getenv("CBSHIM_HASHMAP");
    if (!path) path = "/tmp/keploy-tls/cbmap.txt";

    FILE *f = fopen(path, "r");
    if (!f) {
        DLOG("hashmap not found: %s", path);
        table_n = 0;
        return;
    }

    int n = 0;
    char line[512];
    while (n < MAX_ENTRIES && fgets(line, sizeof line, f)) {
        char mitm_hex[256] = {0};
        char real_hex[256] = {0};
        if (sscanf(line, "%255s %255s", mitm_hex, real_hex) != 2) continue;

        int mlen = hex2bin(mitm_hex, strlen(mitm_hex), table[n].mitm_hash);
        int rlen = hex2bin(real_hex, strlen(real_hex), table[n].real_hash);
        if (mlen <= 0 || rlen <= 0) {
            DLOG("bad hex on line: %s", line);
            continue;
        }
        table[n].mitm_len = (unsigned)mlen;
        table[n].real_len = (unsigned)rlen;
        n++;
    }
    fclose(f);
    table_n = n;
    DLOG("loaded %d entries from %s", table_n, path);
}

// lookup_swap: given the bytes libpq just computed (H(mitm_cert)), find the
// matching real cert hash in the table. Returns the entry pointer or NULL.
static const struct entry *lookup_swap(const unsigned char *md, unsigned int len) {
    pthread_mutex_lock(&table_mu);
    load_table_locked();
    const struct entry *hit = NULL;
    for (int i = 0; i < table_n; i++) {
        if (table[i].mitm_len == len &&
            memcmp(table[i].mitm_hash, md, len) == 0) {
            hit = &table[i];
            break;
        }
    }
    pthread_mutex_unlock(&table_mu);
    return hit;
}

// ---- function interception ------------------------------------------------

typedef int (*real_X509_digest_t)(const X509 *, const EVP_MD *,
                                  unsigned char *, unsigned int *);

static real_X509_digest_t real_X509_digest = NULL;
static pthread_once_t resolve_once = PTHREAD_ONCE_INIT;

// Path patterns of an already-mapped libcrypto. We scan /proc/self/maps
// to confirm the host process IS using a system libcrypto before we
// dlopen one ourselves — if there's no libcrypto in /proc/self/maps the
// app is using a statically-linked / bundled libcrypto and our dlopen'd
// copy would have a different struct layout, segfaulting on the foreign
// X509 struct. Better to fail cleanly with "could not generate peer
// certificate hash" than crash.
static int system_libcrypto_already_mapped(char *out_so, size_t out_so_sz) {
    FILE *f = fopen("/proc/self/maps", "r");
    if (!f) return 0;
    char line[512];
    int hit = 0;
    while (fgets(line, sizeof line, f)) {
        const char *p = strstr(line, "libcrypto.so");
        if (!p) continue;
        // Walk to end of token (whitespace or newline).
        const char *end = p;
        while (*end && *end != ' ' && *end != '\n' && *end != '\t') end++;
        // Walk back to the path start (the line is "addr-addr perm offset dev inode path").
        const char *start = p;
        while (start > line && start[-1] != ' ' && start[-1] != '\t') start--;
        size_t plen = (size_t)(end - start);
        if (plen >= out_so_sz) plen = out_so_sz - 1;
        memcpy(out_so, start, plen);
        out_so[plen] = '\0';
        hit = 1;
        break;
    }
    fclose(f);
    return hit;
}

static void do_resolve(void) {
    // Fast path: RTLD_NEXT walks the dynamic-linker chain skipping the
    // shim itself. For apps that dynamically link libcrypto with the
    // symbols globally visible (most C apps; system libpq users), this
    // returns the next libcrypto's X509_digest directly.
    real_X509_digest = (real_X509_digest_t)dlsym(RTLD_NEXT, "X509_digest");
    if (real_X509_digest) {
        DLOG("real X509_digest resolved via RTLD_NEXT at %p",
             (void *)real_X509_digest);
        return;
    }

    // RTLD_NEXT failed. Two scenarios produce this:
    //
    //   (a) The app DOES link a system libcrypto, but a loader between
    //       the shim and libcrypto used RTLD_LOCAL — symbols aren't in
    //       the global namespace. Python's `import` is the canonical
    //       case (CPython dlopen()s extension modules with RTLD_LOCAL
    //       by default, so libpq → libcrypto pulled in via psycopg2
    //       stays private to that import). The libcrypto IS mapped in
    //       /proc/self/maps; we just need to dlopen it ourselves —
    //       same library, same struct layout, safe to call.
    //
    //   (b) The app statically bundled libcrypto inside a wheel /
    //       single .so (psycopg2-binary is the canonical case).
    //       /proc/self/maps will NOT show a system libcrypto, only
    //       the bundling .so. dlopen'ing a different libcrypto then
    //       calling it with the bundled libcrypto's X509 segfaults
    //       (struct layouts diverge between builds). We must NOT
    //       attempt that — bail with a clear error.
    char libcrypto_path[256];
    if (system_libcrypto_already_mapped(libcrypto_path, sizeof libcrypto_path)) {
        DLOG("found mapped libcrypto at %s — using it for X509_digest", libcrypto_path);
        void *h = dlopen(libcrypto_path, RTLD_LAZY);
        if (!h) {
            fprintf(stderr,
                    "[cbshim] libcrypto is mapped at %s but dlopen() of it failed: %s. "
                    "channel-binding shim disabled.\n",
                    libcrypto_path, dlerror());
            return;
        }
        real_X509_digest = (real_X509_digest_t)dlsym(h, "X509_digest");
        if (real_X509_digest) {
            DLOG("real X509_digest resolved via dlopen(%s) at %p",
                 libcrypto_path, (void *)real_X509_digest);
            // Deliberately leak the handle — we need this for the
            // lifetime of the process.
            return;
        }
        dlclose(h);
        fprintf(stderr,
                "[cbshim] libcrypto at %s has no X509_digest symbol. "
                "channel-binding shim disabled.\n",
                libcrypto_path);
        return;
    }

    fprintf(stderr,
            "[cbshim] cannot resolve X509_digest: no libcrypto mapped in "
            "/proc/self/maps. The app statically linked libcrypto into a "
            "single shared object (psycopg2-binary, oracle instantclient, "
            "some go binaries with cgo+openssl, ...). channel-binding shim "
            "disabled for this process. Switch the app to a "
            "dynamically-linked libcrypto (e.g. psycopg2 source dist, "
            "system python3-psycopg2, apt-installed libpq + libssl3) to "
            "use the shim.\n");
}

int X509_digest(const X509 *cert, const EVP_MD *type,
                unsigned char *md, unsigned int *len) {
    pthread_once(&resolve_once, do_resolve);
    if (!real_X509_digest) return 0;
    (void)cert;
    (void)type;

    int rc = real_X509_digest(cert, type, md, len);
    if (rc != 1 || !md || !len || *len == 0) return rc;

    const struct entry *e = lookup_swap(md, *len);
    if (e) {
        DLOG("swap: digest of len=%u matched a known MITM cert — substituting real hash",
             *len);
        memcpy(md, e->real_hash, e->real_len);
    }
    return rc;
}
