# Connects twice, one after the other, says nothing for 2s on each, and
# reports whether the server's close (it closes connections idle for 1s)
# reached the connection: "eof" if it did, "open" if it still looks open.
# Under keploy the first connection to a new port is examined by the MySQL
# detection probe and the second is not, so the two cover both paths.
import socket
import sys
import time


def once():
    s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), 5)
    time.sleep(2)
    s.settimeout(1)
    try:
        return "eof" if s.recv(16) == b"" else "data"
    except socket.timeout:
        return "open"
    except ConnectionResetError:
        return "eof"
    finally:
        s.close()


print("IDLE", once(), once())
