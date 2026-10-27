# udp_reconnect.py HOST PORT: a UDP socket that first talks to a nameserver,
# then connect()s to the echo server at HOST:PORT, which a UDP socket may do.
# Prints "RECONNECT <first> peer=<getpeername> echoed" for each first exchange:
#   connect  connect() to the nameserver
#   sendto   a datagram sent to it with sendto(), too short to be a DNS
#            message, so keploy's DNS server drops it instead of resolving it
#            upstream (this namespace has none; a failed resolution is an
#            ERROR the lane fails on)
# Under keploy either exchange leaves the nameserver stored for the socket
# (getpeername names it in place of keploy's DNS server); the second connect()
# must drop it, so getpeername names the echo server, as it does without
# keploy.
import errno
import socket
import sys

NAMESERVER = ("127.0.0.2", 53)
ECHO = (sys.argv[1], int(sys.argv[2]))


def reconnect(first):
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.settimeout(5)
    try:
        if first == "connect":
            s.connect(NAMESERVER)
        else:
            s.sendto(b"x", NAMESERVER)
        s.connect(ECHO)
        peer = "%s:%d" % s.getpeername()
        s.send(b"hello-from-a-reconnected-socket\n")
        reply = s.recv(1500)
        return "peer=%s %s" % (peer, "echoed" if reply.startswith(b"ECHO:") else "OTHER")
    except socket.timeout:
        return "TIMEOUT"
    except OSError as e:
        return errno.errorcode.get(e.errno, str(e))
    finally:
        s.close()


for first in ("connect", "sendto"):
    print("RECONNECT", first, reconnect(first), flush=True)
