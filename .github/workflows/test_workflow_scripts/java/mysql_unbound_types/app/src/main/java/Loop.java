import com.sun.net.httpserver.HttpServer;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.sql.Connection;
import java.sql.DriverManager;
import java.sql.PreparedStatement;
import java.sql.ResultSet;

/**
 * The app of the mysql_unbound_types lane (keploy: MySQL EXECUTEs that send no
 * parameter types). It talks to MySQL through Connector/J with server-side
 * prepared statements (useServerPrepStmts=true), on one connection.
 *
 * A client may send a COM_STMT_EXECUTE without its parameter types
 * (new_params_bind_flag 0) when they did not change since the statement's
 * last EXECUTE that sent them; the server reads its values with those.
 *   - GET /loop/{n} executes one prepared statement n times, rebinding an int
 *     and a string each time. Connector/J 8.x sends the types every time;
 *     Connector/J 5.1 only with the first EXECUTE.
 *   - GET /again/{n} binds once and executes n times without rebinding:
 *     every Connector/J sends the types with the first EXECUTE only.
 *   - GET /strings/{n} is /loop/{n} with two strings: a released keploy
 *     recorded those EXECUTEs, every value read as a string, which is what
 *     they are, and stored them with the type FieldTypeString (254).
 *   - GET /small/{n} is /loop/{n} with the integers 1 to n: a released keploy
 *     read a small integer as a string too, its first byte taken for a
 *     length, and recorded those bytes of garbage.
 *
 * Query attributes (a COM_QUERY under CLIENT_QUERY_ATTRIBUTES), which
 * Connector/J 8.x sends with a statement (JdbcStatement.setAttribute, called
 * by reflection so the app still builds with Connector/J 5.1, which has none;
 * these are not requested of it). A released keploy read no attribute's
 * name: it took the name for the value, and glued the value to the head of
 * the query.
 *   - GET /attr/{n}: n queries, each with a traceparent that differs on every
 *     call, as OpenTelemetry's does.
 *   - GET /attrs/{n}: the same with a second attribute, span.
 *   - GET /trace/{n}: n queries with one fixed traceparent.
 *   - GET /attrread/{n}: n queries reading their attribute span
 *     (mysql_query_attribute_string(): the server answers with an attribute
 *     only through it).
 *   - GET /pattr/{n}: one server-prepared statement executed n times, each
 *     with its own value and a traceparent that differs on every call: the
 *     attributes go on the COM_STMT_EXECUTE (a statement keeps its
 *     attributes until they are cleared).
 */
public class Loop {
    static final String FIXED_TRACEPARENT = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01";

    static String traceparent() {
        return "00-" + java.util.UUID.randomUUID().toString().replace("-", "") + "-0000000000000001-01";
    }

    public static void main(String[] args) throws Exception {
        String url = System.getenv().getOrDefault("DB_URL",
            "jdbc:mysql://127.0.0.1:3306/loop?useServerPrepStmts=true&cachePrepStmts=true&useSSL=false&allowPublicKeyRetrieval=true");
        Connection conn = DriverManager.getConnection(url, "root", "password");
        HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 8096), 0);
        server.createContext("/healthz", ex -> { ex.sendResponseHeaders(200, -1); ex.close(); });
        server.createContext("/loop/", ex -> {
            String body;
            int code = 200;
            try {
                int n = Integer.parseInt(ex.getRequestURI().getPath().substring("/loop/".length()));
                StringBuilder sb = new StringBuilder();
                synchronized (conn) {
                    try (PreparedStatement ps = conn.prepareStatement("SELECT ? * 10 AS v, CONCAT(?, '!') AS s")) {
                        for (int i = 1; i <= n; i++) {
                            ps.setInt(1, 1000 + i);
                            ps.setString(2, "item-" + i);
                            try (ResultSet rs = ps.executeQuery()) {
                                rs.next();
                                sb.append(rs.getLong("v")).append(':').append(rs.getString("s")).append(';');
                            }
                        }
                    }
                }
                body = sb.toString();
            } catch (Exception e) {
                code = 500;
                body = "error: " + e;
            }
            byte[] out = body.getBytes(StandardCharsets.UTF_8);
            ex.sendResponseHeaders(code, out.length);
            try (OutputStream os = ex.getResponseBody()) { os.write(out); }
        });
        // GET /again/{n}: binds ONCE and executes n times without rebinding:
        // Connector/J then omits the types from every EXECUTE after the first.
        server.createContext("/again/", ex -> {
            String body;
            int code = 200;
            try {
                int n = Integer.parseInt(ex.getRequestURI().getPath().substring("/again/".length()));
                StringBuilder sb = new StringBuilder();
                synchronized (conn) {
                    try (PreparedStatement ps = conn.prepareStatement("SELECT ? * 10 AS v, CONCAT(?, '!') AS s")) {
                        ps.setInt(1, 2000 + n);
                        ps.setString(2, "again-" + n);
                        for (int i = 1; i <= n; i++) {
                            try (ResultSet rs = ps.executeQuery()) {
                                rs.next();
                                sb.append(rs.getLong("v")).append(':').append(rs.getString("s")).append(';');
                            }
                        }
                    }
                }
                body = sb.toString();
            } catch (Exception e) {
                code = 500;
                body = "error: " + e;
            }
            byte[] out = body.getBytes(StandardCharsets.UTF_8);
            ex.sendResponseHeaders(code, out.length);
            try (OutputStream os = ex.getResponseBody()) { os.write(out); }
        });
        // GET /small/{n}: /loop/{n} with the integers 1 to n.
        server.createContext("/small/", ex -> {
            String body;
            int code = 200;
            try {
                int n = Integer.parseInt(ex.getRequestURI().getPath().substring("/small/".length()));
                StringBuilder sb = new StringBuilder();
                synchronized (conn) {
                    try (PreparedStatement ps = conn.prepareStatement("SELECT ? * 10 AS v, CONCAT(?, '!') AS s")) {
                        for (int i = 1; i <= n; i++) {
                            ps.setInt(1, i);
                            ps.setString(2, "small-" + i);
                            try (ResultSet rs = ps.executeQuery()) {
                                rs.next();
                                sb.append(rs.getLong("v")).append(':').append(rs.getString("s")).append(';');
                            }
                        }
                    }
                }
                body = sb.toString();
            } catch (Exception e) {
                code = 500;
                body = "error: " + e;
            }
            byte[] out = body.getBytes(StandardCharsets.UTF_8);
            ex.sendResponseHeaders(code, out.length);
            try (OutputStream os = ex.getResponseBody()) { os.write(out); }
        });
        // GET /strings/{n}: /loop/{n} with two strings.
        server.createContext("/strings/", ex -> {
            String body;
            int code = 200;
            try {
                int n = Integer.parseInt(ex.getRequestURI().getPath().substring("/strings/".length()));
                StringBuilder sb = new StringBuilder();
                synchronized (conn) {
                    try (PreparedStatement ps = conn.prepareStatement("SELECT CONCAT(?, '-', ?) AS s")) {
                        for (int i = 1; i <= n; i++) {
                            ps.setString(1, "s" + n);
                            ps.setString(2, "item-" + i);
                            try (ResultSet rs = ps.executeQuery()) {
                                rs.next();
                                sb.append(rs.getString("s")).append(';');
                            }
                        }
                    }
                }
                body = sb.toString();
            } catch (Exception e) {
                code = 500;
                body = "error: " + e;
            }
            byte[] out = body.getBytes(StandardCharsets.UTF_8);
            ex.sendResponseHeaders(code, out.length);
            try (OutputStream os = ex.getResponseBody()) { os.write(out); }
        });
        server.createContext("/pattr/", ex -> {
            String body;
            int code = 200;
            try {
                int n = Integer.parseInt(ex.getRequestURI().getPath().substring("/pattr/".length()));
                Class<?> js = Class.forName("com.mysql.cj.jdbc.JdbcStatement");
                java.lang.reflect.Method set = js.getMethod("setAttribute", String.class, Object.class);
                java.lang.reflect.Method clear = js.getMethod("clearAttributes");
                StringBuilder sb = new StringBuilder();
                synchronized (conn) {
                    try (PreparedStatement ps = conn.prepareStatement("SELECT ? * 10 AS v")) {
                        for (int i = 1; i <= n; i++) {
                            clear.invoke(ps.unwrap(js));
                            set.invoke(ps.unwrap(js), "traceparent", traceparent());
                            ps.setInt(1, i);
                            try (ResultSet rs = ps.executeQuery()) {
                                rs.next();
                                sb.append(rs.getLong("v")).append(';');
                            }
                        }
                    }
                }
                body = sb.toString();
            } catch (Exception e) {
                code = 500;
                body = "error: " + e;
            }
            byte[] out = body.getBytes(StandardCharsets.UTF_8);
            ex.sendResponseHeaders(code, out.length);
            try (OutputStream os = ex.getResponseBody()) { os.write(out); }
        });
        for (String path : new String[] {"/attr/", "/attrs/", "/trace/", "/attrread/"}) {
            server.createContext(path, ex -> {
                String body;
                int code = 200;
                try {
                    int n = Integer.parseInt(ex.getRequestURI().getPath().substring(path.length()));
                    Class<?> js = Class.forName("com.mysql.cj.jdbc.JdbcStatement");
                    java.lang.reflect.Method set = js.getMethod("setAttribute", String.class, Object.class);
                    StringBuilder sb = new StringBuilder();
                    synchronized (conn) {
                        for (int i = 1; i <= n; i++) {
                            try (java.sql.Statement st = conn.createStatement()) {
                                Object s = st.unwrap(js);
                                String sql = "SELECT " + i + " AS v";
                                switch (path) {
                                    case "/attr/":
                                        set.invoke(s, "traceparent", traceparent());
                                        break;
                                    case "/attrs/":
                                        set.invoke(s, "traceparent", traceparent());
                                        set.invoke(s, "span", "s" + i);
                                        break;
                                    case "/trace/":
                                        set.invoke(s, "traceparent", FIXED_TRACEPARENT);
                                        break;
                                    default:
                                        set.invoke(s, "span", "s" + i);
                                        sql = "SELECT mysql_query_attribute_string('span') AS v";
                                }
                                try (ResultSet rs = st.executeQuery(sql)) {
                                    rs.next();
                                    sb.append(rs.getString("v")).append(';');
                                }
                            }
                        }
                    }
                    body = sb.toString();
                } catch (Exception e) {
                    code = 500;
                    body = "error: " + e;
                }
                byte[] out = body.getBytes(StandardCharsets.UTF_8);
                ex.sendResponseHeaders(code, out.length);
                try (OutputStream os = ex.getResponseBody()) { os.write(out); }
            });
        }
        server.start();
        System.out.println("listening on 8096");
    }
}
