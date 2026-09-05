// Package consumerfake is the in-repo FAKE protocol for consumer testing.
//
// WHY IT EXISTS. OSS ships no Kafka, Pulsar or SQS parser, so nothing in this
// repository can decode a real broker payload — and the gate, the recorder and
// the judge all sit BEHIND a projector. Without a fake protocol their tests
// could only exercise them through hand-built EffectView values, which tests
// the assertions and not the seam. This package supplies the seam: a projector
// that decodes a payload the tests can write, plus builders that mint mocks
// carrying it, so every consumer test drives the same code path an enterprise
// parser will.
//
// WHY IT IS ITS OWN PACKAGE AND NOT A _test.go FILE. The comparator that
// judges effects lives in pkg/service/replay, so its table-driven test needs
// this projector from a DIFFERENT package — which a _test.go file cannot
// export. A separate package is the httptest arrangement: it is linked only
// into binaries that import it, and no production file does. A guard test in
// the consumer package fails the build if one ever starts to.
package consumerfake

import (
	"encoding/json"
	"fmt"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/consumer"
	"go.keploy.io/server/v3/pkg/models"
)

// Protocol is the fake protocol's registry key. It is deliberately not the
// name of any real protocol, so a fake projector can never shadow a real one.
const Protocol = "fake"

// Kind is the mock Kind whose lower-cased spelling is Protocol. consumer's
// ProtocolOf derives the registry key from a mock's Kind, so the fake has to
// carry a Kind that maps to it — the same coupling a real parser has.
const Kind = models.Kind("Fake")

// SideEffectKind is a second, DIFFERENT mock Kind for standing in as a
// database write or an outgoing call made while handling a message. The
// recorder tells an effect from a side effect by protocol family, so the
// tests need a family that is not the trigger's.
const SideEffectKind = models.Kind("FakeStore")

// payload is what a fake mock carries instead of wire bytes: the views the
// projector should produce, plus the knobs that make it misbehave.
type payload struct {
	Views []models.EffectView `json:"views"`
	// Err, when set, makes the projector return an error — the
	// refuse-don't-guess path a real decoder takes when it meets a wire
	// version it does not model.
	Err string `json:"err,omitempty"`
	// Panic, when set, makes the projector panic. A projector is
	// third-party code from this package's point of view, and a panic in
	// it must be contained and named, not fatal.
	Panic string `json:"panic,omitempty"`
}

// Projector is the fake projector. Register it with Register.
type Projector struct{}

// Project implements consumer.Projector.
func (Projector) Project(m *models.Mock) ([]models.EffectView, error) {
	p, err := decode(m)
	if err != nil {
		return nil, err
	}
	if p.Panic != "" {
		panic(p.Panic)
	}
	if p.Err != "" {
		return nil, fmt.Errorf("fake projector refused this payload: %s", p.Err)
	}
	return p.Views, nil
}

func decode(m *models.Mock) (payload, error) {
	if m == nil {
		return payload{}, fmt.Errorf("fake projector: nil mock")
	}
	if len(m.Spec.GenericResponses) == 0 || len(m.Spec.GenericResponses[0].Message) == 0 {
		return payload{}, fmt.Errorf("fake projector: mock %q carries no payload", m.Name)
	}
	var p payload
	if err := json.Unmarshal([]byte(m.Spec.GenericResponses[0].Message[0].Data), &p); err != nil {
		return payload{}, fmt.Errorf("fake projector: mock %q payload is not decodable: %w", m.Name, err)
	}
	return p, nil
}

// Register registers the fake projector and returns the unregister function.
// Tests defer the unregister so one case cannot leak into the next.
func Register() func() {
	return consumer.RegisterProjector(Protocol, Projector{})
}

// View builds an EffectView with the fields the judge compares.
func View(op, target, key, body string) models.EffectView {
	return models.EffectView{
		Protocol: Protocol,
		Op:       op,
		Target:   target,
		Key:      key,
		Body:     body,
		BodyType: models.BodyType("JSON"),
		Decoded:  models.DecodedConfident,
		Records:  1,
	}
}

// MockOptions describes a fake mock.
type MockOptions struct {
	Name  string
	Kind  models.Kind
	Role  string
	Views []models.EffectView
	// ReqAt / ResAt are the mock's recorded window. ResAt is what the
	// recorder uses as a trigger's delivery instant, and ReqAt is what it
	// uses to detect a produced frame that straddles a unit boundary.
	ReqAt, ResAt time.Time
	ConnID       string
	// Err / Panic make the projector misbehave for this mock.
	Err, Panic string
}

// Mock builds a mock carrying a fake payload.
func Mock(opts MockOptions) *models.Mock {
	kind := opts.Kind
	if kind == "" {
		kind = Kind
	}
	data, err := json.Marshal(payload{Views: opts.Views, Err: opts.Err, Panic: opts.Panic})
	if err != nil {
		// Only reachable if EffectView stops being marshalable, which
		// would break the on-disk format long before it broke this.
		panic(err)
	}
	m := &models.Mock{
		Version:      models.GetVersion(),
		Name:         opts.Name,
		Kind:         kind,
		ConnectionID: opts.ConnID,
		Spec: models.MockSpec{
			Metadata: map[string]string{},
			GenericResponses: []models.Payload{{
				Origin:  models.FromServer,
				Message: []models.OutputBinary{{Type: "json", Data: string(data)}},
			}},
		},
	}
	if opts.Role != "" {
		m.Spec.Metadata[models.MetaKeyRole] = opts.Role
	}
	if len(opts.Views) > 0 {
		m.Spec.Metadata[models.MetaKeyOp] = opts.Views[0].Op
		m.Spec.Metadata[models.MetaKeyTarget] = opts.Views[0].Target
	}
	m.Spec.ReqTimestampMock = opts.ReqAt
	m.Spec.ResTimestampMock = opts.ResAt
	return m
}
