package manager

import (
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// stampOutsideRequestsLocked marks mock as made while no incoming request was
// open (models.Mock.OutsideRequests), when the windows can show it: the call
// began after the latest window ended, no window still open began by its
// answer, and no request given up while in flight is still running (a given-up
// window has left the open ones, but its request may still make calls until
// it ends: noteGivenUpInFlightLocked). A window opens before its request
// reaches the app (OpenWindow), so a request that can have made the call has
// opened one by the time the mock is here, however late the mock's parser
// hands it over. A request that opens none cannot be recorded, so no test owns
// what it calls.
//
// It marks nothing unless a window has ended here, so an agent whose ingress
// opens none, the proxyless one, says nothing, and nothing once a request that
// opens no window has come (NoteWindowlessIngress): the windows no longer cover
// the requests. Nor a call missing either of its times, which the checks below
// cannot place. Caller holds mu.
func (m *SyncMockManager) stampOutsideRequestsLocked(mock *models.Mock) {
	if mock == nil || m.windowlessIngress || m.lastWindowEnd.IsZero() || m.givenUpInFlight > 0 {
		return
	}
	req, res := mock.Spec.ReqTimestampMock, mock.Spec.ResTimestampMock
	if res.IsZero() || !m.lastWindowEnd.Before(req) {
		return
	}
	if len(m.open) > 0 && !m.open[0].start.After(res) {
		return
	}
	mock.OutsideRequests = true
}

// noteGivenUpInFlightLocked notes that w has just left the open windows given
// up (fitHoldLocked, giveUpForPool, giveUpForPressureLocked,
// endStopFoundLocked) while its request is still in flight: until that request
// ends (Close, givenUpEndedLocked), no mock is marked outside the requests. A
// given-up window whose Close never comes leaves the marking off for the
// manager's life, which marks nothing it should not. Caller holds mu.
func (m *SyncMockManager) noteGivenUpInFlightLocked(w *Window) {
	w.inFlight = true
	m.givenUpInFlight++
}

// givenUpEndedLocked says the request of w, a window given up while it was in
// flight, has ended: it is the latest window to end. Caller holds mu.
func (m *SyncMockManager) givenUpEndedLocked(w *Window) {
	w.inFlight = false
	m.givenUpInFlight--
	m.lastWindowEnd = time.Now()
}

// NoteWindowlessIngress says this agent's ingress took a request that opens no
// window and that a recording can keep, a gRPC call: from now on no mock is
// marked outside the requests (stampOutsideRequestsLocked). It stays so for the
// manager's life, as such requests leave no trace here to tell a later call's
// span from theirs.
func (m *SyncMockManager) NoteWindowlessIngress() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.windowlessIngress = true
	m.mu.Unlock()
}
