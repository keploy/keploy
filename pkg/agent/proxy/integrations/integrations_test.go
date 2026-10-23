package integrations

import (
	"testing"

	"go.uber.org/zap"
)

// A build skips Redis (the open-source keploy) unless it links a Redis parser
// (registered as REDIS) or a package claims Redis (Keploy Enterprise, on every
// platform, its parser linked there or not); a claim taken back skips again.
func TestSkipsRedis(t *testing.T) {
	if _, ok := Registered[REDIS]; ok {
		t.Fatal("precondition: this build registers a Redis parser")
	}
	if !SkipsRedis() {
		t.Fatal("a build with no Redis parser and no claim does not skip Redis")
	}
	release := ClaimRedis()
	if SkipsRedis() {
		release()
		t.Fatal("a build that claims Redis skips it")
	}
	// A claim made again, as a test does in a build that claims Redis
	// already, puts it back as it was: claimed.
	again := ClaimRedis()
	again()
	if SkipsRedis() {
		release()
		t.Fatal("taking back a second claim took back the first")
	}
	release()
	if !SkipsRedis() {
		t.Fatal("a claim taken back still keeps Redis from being skipped")
	}
	Register(REDIS, &Parsers{Initializer: func(*zap.Logger) Integrations { return nil }, Priority: 200})
	t.Cleanup(func() { delete(Registered, REDIS) })
	if SkipsRedis() {
		t.Fatal("a build that links a Redis parser skips Redis")
	}
}
