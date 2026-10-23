// The test command of the redis_skip lane's keploy mock check: it makes the
// calls a test suite would, a SET and a GET on Redis (REDIS_ADDR) and a get
// on memcached (CACHE_ADDR), and exits 0 when each is answered as expected:
// the GET with the value the SET wrote, the memcached get with a reply that
// ends in END. With LOOKUP set, it first looks that name up, as a suite
// whose replay asks for a name its recording never looked up, and fails if
// the name has no address.
package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/redis/go-redis/v9"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "client:", err)
		os.Exit(1)
	}
	fmt.Println("client: ok")
}

func run() error {
	ctx := context.Background()
	if name := os.Getenv("LOOKUP"); name != "" {
		addrs, err := net.DefaultResolver.LookupHost(ctx, name)
		if err != nil {
			return fmt.Errorf("lookup %s: %w", name, err)
		}
		fmt.Printf("client: %s has %s\n", name, strings.Join(addrs, ", "))
	}
	rdb := redis.NewClient(&redis.Options{Addr: os.Getenv("REDIS_ADDR")})
	defer func() { _ = rdb.Close() }()
	if err := rdb.Set(ctx, "mock:k", "mocked", 0).Err(); err != nil {
		return fmt.Errorf("redis SET: %w", err)
	}
	got, err := rdb.Get(ctx, "mock:k").Result()
	if err != nil {
		return fmt.Errorf("redis GET: %w", err)
	}
	if got != "mocked" {
		return fmt.Errorf("redis GET answered %q, want %q", got, "mocked")
	}
	c, err := net.Dial("tcp", os.Getenv("CACHE_ADDR"))
	if err != nil {
		return fmt.Errorf("memcached: %w", err)
	}
	defer func() { _ = c.Close() }()
	if _, err := fmt.Fprint(c, "get mock:none\r\n"); err != nil {
		return fmt.Errorf("memcached get: %w", err)
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return fmt.Errorf("memcached get: %w", err)
	}
	if !strings.HasSuffix(line, "END\r\n") {
		return fmt.Errorf("memcached get answered %q, want END", line)
	}
	return nil
}
