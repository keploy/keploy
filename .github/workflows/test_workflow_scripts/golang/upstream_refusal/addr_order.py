# addr_order.py NAME: the order getaddrinfo gives NAME's addresses in, by
# family ("ORDER v4,v6"). glibc sorts them by RFC 6724, UDP-connect()ing to
# each to find the ones with no route, which it puts last.
import socket
import sys

families = ["v6" if ai[0] == socket.AF_INET6 else "v4"
            for ai in socket.getaddrinfo(sys.argv[1], 80, type=socket.SOCK_STREAM)]
print("ORDER " + ",".join(families), flush=True)
