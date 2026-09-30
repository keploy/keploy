package record

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// handingMockDB hands back, for each InsertMock, the documents in hand: for
// the mock inserted, or, when other is set, for a companion mock a wrapping
// MockDB writes on the same context.
type handingMockDB struct {
	hand []handedDoc
	err  error
}

type handedDoc struct {
	doc   []byte
	other bool
}

func (h *handingMockDB) InsertMock(ctx context.Context, m *models.Mock, _ string) error {
	for _, d := range h.hand {
		target := m
		if d.other {
			target = &models.Mock{Name: "companion"}
		}
		models.HandMockDoc(ctx, target, d.doc, "yaml")
	}
	return h.err
}

// InsertedMockDoc returns a copy of the document InsertMock handed back for the
// mock it inserted, which the hooks may keep, and costs that copy and nothing
// else; nothing for a mock with no document, one InsertMock failed, one whose
// document was handed for another mock, or one handed two documents (written
// to two stores: which is the mocks file's?); and it installs no receiver for
// the no-op hooks, which read nothing.
func TestInsertedMockDoc(t *testing.T) {
	bg := context.Background()
	d := NewInsertedMockDoc(bg, &encodedHooks{})

	written := []byte("kind: MySQL\n")
	db := &handingMockDB{hand: []handedDoc{{doc: written}}}
	first, format, err := d.Insert(db, &models.Mock{}, "s")
	require.NoError(t, err)
	require.Equal(t, "kind: MySQL\n", string(first))
	require.Equal(t, "yaml", format)
	// The MockDB reuses its buffer once InsertMock returns.
	copy(written, "XXXXXXXXXXXX")
	require.Equal(t, "kind: MySQL\n", string(first), "the document returned is the MockDB's buffer, not a copy")

	// A companion's document, handed after this mock's, is not this mock's.
	doc, _, err := d.Insert(&handingMockDB{hand: []handedDoc{{doc: []byte("kind: MySQL\n")}, {doc: []byte("kind: Other\n"), other: true}}}, &models.Mock{}, "s")
	require.NoError(t, err)
	require.Equal(t, "kind: MySQL\n", string(doc))

	for name, db := range map[string]*handingMockDB{
		"no document":                 {},
		"an empty document":           {hand: []handedDoc{{doc: []byte{}}}},
		"InsertMock failed":           {hand: []handedDoc{{doc: []byte("kind: MySQL\n")}}, err: errors.New("no space left on device")},
		"a companion's document only": {hand: []handedDoc{{doc: []byte("kind: Other\n"), other: true}}},
		"two for this mock":           {hand: []handedDoc{{doc: []byte("kind: MySQL\n")}, {doc: []byte("{\"kind\":\"MySQL\"}\n")}}},
	} {
		doc, format, err := d.Insert(db, &models.Mock{}, "s")
		require.Nil(t, doc, name)
		require.Empty(t, format, name)
		require.Equal(t, db.err, err, name)
	}
	require.Equal(t, "kind: MySQL\n", string(first), "a later insert changed a document a hook kept")

	big := bytes.Repeat([]byte("x"), 27<<10)
	many := &handingMockDB{hand: []handedDoc{{doc: big}}}
	mock := &models.Mock{}
	var got []byte
	allocs := testing.AllocsPerRun(100, func() {
		got, _, _ = d.Insert(many, mock, "s")
	})
	require.Equal(t, 1.0, allocs, "handing a 27 KB document on should cost its copy and nothing else")
	require.Equal(t, big, got)

	noop := NewInsertedMockDoc(bg, BaseRecordHooks{})
	require.Equal(t, bg, noop.ctx, "the no-op hooks got a receiver: every mock's document would be copied for nothing")
	doc, _, err = noop.Insert(&handingMockDB{hand: []handedDoc{{doc: []byte("kind: MySQL\n")}}}, &models.Mock{}, "s")
	require.NoError(t, err)
	require.Nil(t, doc)
}
