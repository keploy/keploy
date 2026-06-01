# App container. Stand-in for sphinx — a libpq-using app on the
# default Debian + glibc + OpenSSL 3.x target. Compiles both:
#
#   - client    : a tiny libpq probe that connects via SCRAM-SHA-256-PLUS
#                 and prints what it observed
#   - cbshim.so : the LD_PRELOAD shim that intercepts X509_digest
#
# Compiled in-image so the shim is ABI-matched to this container's
# OpenSSL + glibc — the production deployment does the same thing
# (keploy ships a precompiled shim that targets the dominant
# Debian/glibc/OpenSSL3 combination).
FROM debian:13-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential libpq-dev libssl-dev libpq5 ca-certificates && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /build
COPY client.c cbshim.c .

RUN gcc -O0 -g -Wall -I/usr/include/postgresql client.c -lpq -o /usr/local/bin/client && \
    gcc -O0 -g -Wall -Wextra -fPIC -shared -o /usr/local/lib/cbshim.so cbshim.c -ldl -lcrypto -lpthread && \
    rm -rf /build

# Wait for cbmap to exist before running — the MITM proxy needs to
# accept the first connection and complete its upstream TLS handshake
# before publishing. We give it a generous timeout to handle slow CI.
# (Without this the very first run sees an empty cbmap and falls through
# to FATAL — only the SECOND attempt would pass. The wait makes the
# test single-shot deterministic.)
COPY wait-and-run.sh /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/wait-and-run.sh"]
