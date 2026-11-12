import java.io.InputStream;
import java.io.OutputStream;
import java.net.Socket;
import java.net.URI;
import java.nio.charset.StandardCharsets;

// HeldCall URL is a JVM client of the upstream-faults e2e: it sends a GET of
// URL on a new connection, prints "sent", and waits for the answer, which the
// upstream never gives. The JVM handles SIGTERM itself, as it does by
// default: it runs its shutdown hooks (this one prints "jvm: stopping") and
// exits with status 143, the socket still open, so its exit releases it.
public class HeldCall {
    public static void main(String[] args) throws Exception {
        URI u = new URI(args[0]);
        Runtime.getRuntime().addShutdownHook(new Thread(() -> System.out.println("jvm: stopping")));
        Socket s = new Socket(u.getHost(), u.getPort());
        OutputStream out = s.getOutputStream();
        String req = "GET " + u.getRawPath() + "?" + u.getRawQuery() + " HTTP/1.1\r\nHost: " + u.getHost() + ":" + u.getPort() + "\r\n\r\n";
        out.write(req.getBytes(StandardCharsets.US_ASCII));
        out.flush();
        System.out.println("sent");
        System.out.flush();
        InputStream in = s.getInputStream();
        System.out.println("jvm: answered " + in.read());
    }
}
