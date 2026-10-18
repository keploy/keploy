package replay

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// publishingInstrumentation answers PublishedHostPort from answers, keyed by
// app port, and counts the questions.
type publishingInstrumentation struct {
	Instrumentation
	answers map[uint16]struct {
		port  uint16
		known bool
	}
	asked int
}

func (p *publishingInstrumentation) PublishedHostPort(_ context.Context, _ string, appPort uint16) (uint16, bool) {
	p.asked++
	a := p.answers[appPort]
	return a.port, a.known
}

func publishing(answers map[uint16][2]uint16) *publishingInstrumentation {
	p := &publishingInstrumentation{answers: map[uint16]struct {
		port  uint16
		known bool
	}{}}
	for app, a := range answers {
		p.answers[app] = struct {
			port  uint16
			known bool
		}{a[0], a[1] == 1}
	}
	return p
}

// The instrumentation is asked once a test set for each address and app
// port, and asked again when it could not tell; each redirect is said once a
// run, at Info, whatever the sets; the replayer and its hooks, which each hold
// the instrumentation, share one.
func TestPublishedPortsAskOnceASetAndSayOnceARun(t *testing.T) {
	inst := publishing(map[uint16][2]uint16{8080: {18080, 1}, 9090: {0, 1}, 7070: {0, 0}})
	core, logs := observer.New(zapcore.InfoLevel)
	p := publishedPortsOf(zap.New(core), inst)
	require.Same(t, p, publishedPortsOf(zap.NewNop(), inst), "the replayer and its hooks share one")
	ctx := context.Background()

	set0 := p.of("test-set-0")
	require.Equal(t, uint16(18080), set0(ctx, "localhost", 8080))
	require.Equal(t, uint16(18080), set0(ctx, "localhost", 8080))
	require.Equal(t, uint16(0), set0(ctx, "localhost", 9090))
	require.Equal(t, 2, inst.asked, "once a set for each app port")
	require.Equal(t, uint16(0), set0(ctx, "localhost", 7070))
	require.Equal(t, uint16(0), set0(ctx, "localhost", 7070))
	require.Equal(t, 4, inst.asked, "an answer it could not give is asked for again")

	set1 := p.of("test-set-1")
	require.Equal(t, uint16(18080), set1(ctx, "localhost", 8080))
	require.Equal(t, 5, inst.asked, "a new set asks again: its agent container is another")

	var said []string
	for _, e := range logs.All() {
		said = append(said, e.Message)
	}
	require.Equal(t, []string{"app port 8080 is published on host port 18080; sending these tests there"}, said)
}

// An instrumentation that cannot tell where the app's ports are published
// leaves the tests where they were recorded: nothing is asked.
func TestNoPublishedPortsWithoutAnInstrumentationThatTells(t *testing.T) {
	type plain struct{ Instrumentation }
	require.Nil(t, publishedPortsOf(zap.NewNop(), plain{}))
	require.Nil(t, publishedPortsOf(zap.NewNop(), plain{}).of("test-set-0"))
}

// Each run of a set asks again, an attempt of the same set too (the agent
// container it starts may publish the app's port elsewhere), and an answer to
// a question an earlier run asked is not kept for the next.
func TestEachRunOfASetAsksAgain(t *testing.T) {
	inst := publishing(map[uint16][2]uint16{8080: {18080, 1}})
	p := &publishedPorts{pub: inst, logger: zap.NewNop(), ports: map[string]uint16{}, said: map[[2]uint16]bool{}}
	ctx := context.Background()
	p.newRun("test-set-0")
	require.Equal(t, uint16(18080), p.of("test-set-0")(ctx, "localhost", 8080))
	require.Equal(t, uint16(18080), p.of("test-set-0")(ctx, "localhost", 8080))
	require.Equal(t, 1, inst.asked)

	// A second attempt of the set, its agent now publishing 28080.
	inst.answers[8080] = struct {
		port  uint16
		known bool
	}{28080, true}
	p.newRun("test-set-0")
	require.Equal(t, uint16(28080), p.of("test-set-0")(ctx, "localhost", 8080))
	require.Equal(t, 2, inst.asked, "an attempt of the same set asks again")

	// An answer that comes back after the next run began is not kept for it.
	slow := &racingPublishing{answer: 18080, begin: func() { p.newRun("test-set-0") }}
	p.pub = slow
	p.newRun("test-set-0")
	require.Equal(t, uint16(18080), p.of("test-set-0")(ctx, "localhost", 9090))
	p.mu.Lock()
	_, kept := p.ports["localhost 9090"]
	p.mu.Unlock()
	require.False(t, kept, "an earlier run's answer was kept for the run that began while it was asked")
}

// racingPublishing answers after calling begin, as a new run that starts while
// the question is out does.
type racingPublishing struct {
	answer uint16
	begin  func()
}

func (r *racingPublishing) PublishedHostPort(context.Context, string, uint16) (uint16, bool) {
	r.begin()
	return r.answer, true
}
