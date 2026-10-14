# Connect results through keploy's proxy

While recording — and in `keploy test` whenever calls reach real dependencies
(mocking off, global passthrough) — keploy's
eBPF connect hook points each outgoing connection at keploy's proxy, which
dials the real destination for it. The proxy now holds the connection's TCP
handshake until that dial has an answer, so the application's `connect()`
returns what it would have returned without keploy:

| The destination...                 | The application's connect gets |
| ---------------------------------- | ------------------------------ |
| accepts                            | success, when it accepted      |
| refuses                            | `ECONNREFUSED`                 |
| is unreachable (no route / ARP)    | `ENETUNREACH` / `EHOSTUNREACH` |
| never answers                      | no answer: the app's own timeout |

Before, every connect succeeded at once and a dependency that was down showed
up as EOF on the first read, so retry-on-refused logic in drivers never ran.

The connection the proxy dialled while holding the handshake is the one it
then uses for the application's connection, rather than dialling the
destination again.

A destination that closes it before the application has sent anything (an
idle timeout) closes the application's connection too. If the application
has sent something (a TLS ClientHello, which keploy answers itself), its
connection stays open and keploy dials the destination again when the first
request arrives, as it did before.

To turn it off: `--disable-handshake-hold` on `keploy record`, `keploy test`,
`keploy mock record` or `keploy mock replay`, or `disableHandshakeHold: true`
under `record:` in `keploy.yml`.

## How

- The agent adds an nf_tables table `inet keploy_synhold_<proxy port>` in its
  network namespace (the host for a native run, the agent container for a
  docker one). Its rules queue SYNs to the proxy port that arrive over
  loopback (NFQUEUE number = proxy port, with `bypass`, so nothing is held
  if the agent is gone) and answer refusals with a kernel `reject`.
- The destination is found from the connecting socket (sock_diag) and the
  connect hook's record for it.
- The table is removed when the agent stops; a table left by a killed agent
  is removed by the next agent that starts.

Kernel needs (Linux 5.10+, amd64 and arm64): `nf_tables`, `nft_queue`,
`nft_reject_inet`, `nfnetlink_queue`, `inet_diag` and `tcp_diag` (loaded on
demand), and `CAP_NET_ADMIN`, which the agent already runs with.

## When it is not available

At start keploy checks, with a probe connection of its own over each loopback
(IPv4, and IPv6 where `::1` exists), that a held handshake can be refused on
this kernel. If that check fails, or any of the
above is missing, the agent logs once, at Info, `could not hold the
application's connections until their destination answers`, with the reason,
and handshakes complete at once as they did before.

## Not affected

- Replay with mocks: no real dependency is dialled, so keploy holds nothing,
  adds no rules and loads no modules; handshakes complete at once as before.
- Observe-only / low-latency capture: connections are not redirected to a
  proxy.

## Behaviour changes

A dependency that refuses (or cannot be reached) while recording now fails
the application's connect, and keploy logs it once a minute per dependency at
Info; nothing is recorded for that connection. When the same test is
replayed with mocks, the connection is accepted (there is no dependency to
ask) and the application finds no mock for it, so a test whose response
depends on the connect error's text can differ between record and replay.
Before, both saw EOF.

Every recorded connection now reaches its dependency when the application
connects, as it would without keploy, even if the application never sends a
byte on it; keploy used to dial only when the first bytes arrived. And a TLS
dependency is reached at the address the application connected to, rather
than by resolving the server name it sent again. If a TLS dependency closes
or resets that connection when keploy starts its handshake on it, keploy
dials once more, so such a dependency sees two connections where it saw one.

A connection to `[::1]` whose dependency listens only on IPv4 used to be
dialled on `127.0.0.1` by keploy instead. It is now refused, as it is
without keploy; an application that resolved `localhost` to both addresses
falls back to `127.0.0.1` itself, and one that connects to `::1` literally
(or a runtime that tries only the first address `localhost` resolves to) gets
`ECONNREFUSED` — on hosts where the hold is active.

## Cost

Each recorded connect waits for keploy to answer it from userspace and for
the dependency to answer keploy: a connect completes when the dependency
accepts, not before. An agent that is stopped (SIGSTOP) or starved of CPU
while recording delays connects that the kernel used to complete alone; a
killed agent does not (its queue is unbound, and the rule lets SYNs through).

The unreachable answers are ICMP messages, which the kernel rate-limits (on
older kernels also over loopback): in a burst of more than about fifty
unreachable connects at once, some of them fail after a SYN retransmission
(a second or more) rather than at once. The answer is the same.

## Side effects on the host's netfilter

Removing a netfilter hook makes the kernel drop the packets queued in any
NFQUEUE of that network namespace. So when keploy stops (or removes a table
a killed agent left behind), a packet another NFQUEUE user (an IPS, an
application firewall) holds at that instant is dropped and retransmitted by
its sender, as happens whenever any software removes a base chain.
