package http

import (
	"context"
	"path/filepath"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.uber.org/zap"
)

// The release gate of word-level learn (design r9 §7), in process: record's
// full round learns the words of "ORD-1 paid 100 ref=<hex>" that changed
// (learnMatch), the consumed mock's state is written onto the recording
// (mockdb's PersistLearned) and read back from disk, and a later replay's
// strict serve answers a fresh ref and refuses "paid 999".
func TestReleaseGateLearnPersistServe(t *testing.T) {
	h, n, opts := learnOn(t)
	const body, bodyF = `{"note":"ORD-1 paid 100 ref=3f2b8c1e9a4d","item":"book"}`, `{"note":"ORD-1 paid 100 ref=a7c4e2d10b3f","item":"book"}`
	rec := learnRecorded("mock-0", 0, "POST", "/v1/orders", body, nil, `{"ok":true}`)

	// The recording, on disk.
	dir := t.TempDir()
	ys := mockdb.New(zap.NewNop(), dir, "")
	ctx := context.Background()
	set := "test-set-0"
	onDisk := *rec
	onDisk.Name = ""
	if err := ys.InsertMock(ctx, &onDisk, set); err != nil {
		t.Fatal(err)
	}
	if err := ys.Close(); err != nil {
		t.Fatal(err)
	}

	// F: the learn step learns the ref's words.
	db := &learnDb{perTest: []*models.Mock{rec}}
	if served, miss, err := h.learnMatch(ctx, live("POST", "/v1/orders", bodyF, nil), db, n, opts); err != nil || served == nil || miss != nil {
		t.Fatalf("F: served %v miss %+v err %v", served, miss, err)
	}
	c := db.consumed[0]
	state := models.MockState{Name: c.Name, Kind: c.Kind, LearnedWords: c.Spec.LearnedWords, LearnedF: c.Spec.LearnedF()}
	if added, err := ys.PersistLearned(ctx, set, map[string]models.MockState{c.Name: state}); err != nil || len(added["mock-0"]) != 1 {
		t.Fatalf("persisted %v (%v)", added, err)
	}
	if err := ys.WriteLearnedFiles(set, map[string]models.MockState{c.Name: state}); err != nil {
		t.Fatal(err)
	}

	// CI: the recording as read back serves only what F saw change.
	mocks, err := mockdb.ReadSetMocks(ctx, zap.NewNop(), filepath.Join(dir, set))
	if err != nil || len(mocks) != 1 || len(mocks[0].Spec.LearnedWords) != 1 || len(mocks[0].Spec.ReqBodyNoise) != 0 {
		t.Fatalf("read back %+v (%v)", mocks, err)
	}
	read := mocks[0]
	read.Spec.HTTPReq.Header["Host"] = "api.example"
	for _, tc := range []struct {
		body string
		ok   bool
	}{
		{`{"note":"ORD-1 paid 100 ref=0c9d8e7f6a5b","item":"book"}`, true},
		{`{"note":"ORD-1 paid 999 ref=0c9d8e7f6a5b","item":"book"}`, false},
		{`{"note":"ORD-2 paid 100 ref=0c9d8e7f6a5b","item":"book"}`, false},
		{`{"note":"ORD-1 paid 100 ref=0c9d8e7f6a5b","item":"pen"}`, false},
	} {
		in := sent("/v1/orders", map[string]string{"Content-Type": "application/json"}, tc.body)
		served, _ := strictMatch(t, in, nil, nil, nil, read.DeepCopy())
		if (served != nil) != tc.ok {
			t.Errorf("%s: served %v, want %v", tc.body, served != nil, tc.ok)
		}
	}
}

// Backward compatibility (compat audit A6, r9 §7): with the switch off, a
// recording's learned words change nothing: every call is served or refused
// as the same recording with no words is, by the matcher main has.
func TestTheSwitchOffServesAWordsCarryingRecordingAsMainDoes(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	const body = `{"note":"ORD-1 paid 100 ref=3f2b8c1e9a4d","item":"book"}`
	plain := func() *models.Mock {
		return recorded("mock-0", "/v1/orders", map[string]string{"Content-Type": "application/json"}, body)
	}
	worded := func() *models.Mock {
		return withWords(t, plain(), "body.note", "ORD-1 paid 100 ref=3f2b8c1e9a4d", "ORD-1 paid 100 ref=a7c4e2d10b3f")
	}
	for _, b := range []string{
		body,
		`{"note":"ORD-1 paid 100 ref=0c9d8e7f6a5b","item":"book"}`,
		`{"note":"ORD-1 paid 999 ref=0c9d8e7f6a5b","item":"book"}`,
		`{"note":"ORD-1 paid 100 ref=0c9d8e7f6a5b","item":"pen"}`,
		`{"other":1}`,
	} {
		var got [2]bool
		for i, m := range []*models.Mock{plain(), worded()} {
			db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}
			ok, _, _, err := newHTTP().match(context.Background(), sent("/v1/orders", map[string]string{"Content-Type": "application/json"}, b), db, nil, nil, nil, true, false, false, true, true)
			if err != nil {
				t.Fatal(err)
			}
			got[i] = ok
		}
		if got[0] != got[1] {
			t.Errorf("%s: served %v with no words, %v with words: the switch off read them", b, got[0], got[1])
		}
	}
}
