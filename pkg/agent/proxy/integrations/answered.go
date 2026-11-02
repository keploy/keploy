package integrations

import (
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/models"
)

// answerWritten, when set, is told of each recorded mock whose answer a
// parser has written to the application (SetAnswerWritten).
var answerWritten atomic.Pointer[func(*models.Mock)]

// SetAnswerWritten makes f told, from now on, of each recorded mock whose
// answer a parser wrote to the application, once the write returned (nil:
// none). It is the event a deliver: entry waits for (models.Delivery.After):
// told at the consume, a callback could reach the application before it read
// the answer that triggers it. Only the HTTP parser's matched answer tells it
// today; models.ValidateDeliver refuses an after: of any other kind. f runs
// on the parser's goroutine, holding no lock of keploy's, and must not block.
func SetAnswerWritten(f func(*models.Mock)) {
	if f == nil {
		answerWritten.Store(nil)
		return
	}
	answerWritten.Store(&f)
}

// AnswerWritten tells the SetAnswerWritten listener, if any, that mk's answer
// was written to the application.
func AnswerWritten(mk *models.Mock) {
	if f := answerWritten.Load(); f != nil && mk != nil {
		(*f)(mk)
	}
}
