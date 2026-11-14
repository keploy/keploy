package proxy

import (
	"bufio"
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"os"

	"go.keploy.io/server/v3/pkg/models"
)

// maxSpilledMissBytes bounds what one list of misses keeps on the agent's
// disk past its room in memory (missSpill): 64 MiB per test's window, so
// neither the agent's memory nor its disk grows without limit, however many
// distinct calls a test makes that no recording answers.
var maxSpilledMissBytes int64 = 64 << 20

// missSpill is where a testErrorAccumulator keeps the misses past its room
// that are alike to none it holds (foldAlike): a file of its own in the
// agent's temp folder, removed from the folder as it is made, so it is gone
// when the list is drained and when the agent exits, even killed. Its misses
// are read back, in the order they were kept, when the list is drained: at
// the window's close and at the read. Nil until a miss is first kept on it.
type missSpill struct {
	f *os.File
	// path is the file's name while it is still in the folder (a system
	// that cannot remove an open file): removed at close.
	path string
	enc  *gob.Encoder
	buf  bytes.Buffer
	// n is how many misses the file holds, size how many bytes.
	n    int
	size int64
	// full is why the file takes no more misses (its bound reached, or the
	// disk refused one): every miss past it is dropped, and the read is
	// incomplete, naming it.
	full string
}

// The marks a spilled miss was filed with (spilledMiss.Marks).
const (
	spillMarkBefore uint8 = iota + 1
	spillMarkEarlier
)

// spilledMiss is one miss as its spill keeps it: its parser error, and the
// marks it was filed with, outermost first (beforeSetMiss, earlierSetMiss).
// The parser error's Err is kept as its text and whether it was a miss no
// mock matched (models.ErrNoMockMatched): what a read of it reads. Every
// other field, the report's own unwritten ones (json:"-") among them, is kept
// as it was; an empty list is read back as none.
type spilledMiss struct {
	Marks         []uint8
	PE            models.ParserError
	HasErr        bool
	ErrText       string
	NoMockMatched bool
}

// spilledErr is a spilled miss's Err as read back: its text, and that it
// is a miss no mock matched when it was one.
type spilledErr struct {
	text          string
	noMockMatched bool
}

func (e spilledErr) Error() string { return e.text }

// Is reports whether e was a miss no mock matched, for errors.Is.
func (e spilledErr) Is(target error) bool {
	return e.noMockMatched && target == models.ErrNoMockMatched
}

// toSpilled is err as a spill keeps it; ok is false for an err that is no
// parser's miss (models.ErrMockNotFound), bare or marked as filed: no read
// lists one, so none is kept.
func toSpilled(err error) (spilledMiss, bool) {
	var s spilledMiss
	for {
		if b, isBefore := err.(beforeSetMiss); isBefore {
			s.Marks, err = append(s.Marks, spillMarkBefore), b.error
			continue
		}
		if e, isEarlier := err.(earlierSetMiss); isEarlier {
			s.Marks, err = append(s.Marks, spillMarkEarlier), e.error
			continue
		}
		break
	}
	pe, ok := err.(models.ParserError)
	if !ok || pe.ParserErrorType != models.ErrMockNotFound {
		return spilledMiss{}, false
	}
	if pe.Err != nil {
		s.HasErr, s.ErrText = true, pe.Err.Error()
		s.NoMockMatched = errors.Is(pe.Err, models.ErrNoMockMatched)
	}
	pe.Err = nil
	s.PE = pe
	return s, true
}

// miss is s as the miss it was filed as.
func (s spilledMiss) miss() error {
	pe := s.PE
	if s.HasErr {
		pe.Err = spilledErr{text: s.ErrText, noMockMatched: s.NoMockMatched}
	}
	var err error = pe
	for i := len(s.Marks) - 1; i >= 0; i-- {
		if s.Marks[i] == spillMarkBefore {
			err = beforeSetMiss{err}
		} else {
			err = earlierSetMiss{err}
		}
	}
	return err
}

// keep writes err to the spill, making its file on the first. It returns
// the miss's place on it, and false when it was not kept: err is no
// parser's miss, or the spill is full (s.full says why).
func (s *missSpill) keep(err error) (int, bool) {
	if s.full != "" {
		return 0, false
	}
	rec, ok := toSpilled(err)
	if !ok {
		return 0, false
	}
	if s.f == nil {
		f, cerr := os.CreateTemp("", "keploy-misses-*.gob")
		if cerr != nil {
			s.full = "the agent could not make a file for them: " + cerr.Error()
			return 0, false
		}
		s.f, s.path = f, f.Name()
		if os.Remove(s.path) == nil {
			s.path = ""
		}
		s.enc = gob.NewEncoder(&s.buf)
	}
	s.buf.Reset()
	if eerr := s.enc.Encode(rec); eerr != nil {
		s.full = "the agent could not write one: " + eerr.Error()
		return 0, false
	}
	if s.size+int64(s.buf.Len()) > maxSpilledMissBytes {
		s.full = fmt.Sprintf("they reached the %d MiB the agent keeps of them on its disk", maxSpilledMissBytes>>20)
		return 0, false
	}
	if _, werr := s.f.Write(s.buf.Bytes()); werr != nil {
		s.full = "the agent's disk refused them: " + werr.Error()
		return 0, false
	}
	s.size += int64(s.buf.Len())
	s.n++
	return s.n - 1, true
}

// readBack is every miss the spill kept, in the order it kept them, and
// closes its file. When its file cannot be read back whole, it returns the
// misses it read and why the rest are lost. A nil spill kept none.
func (s *missSpill) readBack() ([]error, string) {
	if s == nil || s.f == nil {
		return nil, ""
	}
	defer s.close()
	if _, err := s.f.Seek(0, 0); err != nil {
		return nil, fmt.Sprintf("the %d misses kept on the agent's disk could not be read back: %v", s.n, err)
	}
	dec := gob.NewDecoder(bufio.NewReader(s.f))
	out := make([]error, 0, s.n)
	for i := 0; i < s.n; i++ {
		var rec spilledMiss
		if err := dec.Decode(&rec); err != nil {
			return out, fmt.Sprintf("%d of the %d misses kept on the agent's disk could not be read back: %v", s.n-i, s.n, err)
		}
		out = append(out, rec.miss())
	}
	return out, ""
}

// close closes the spill's file, removing it from the folder where it is
// still there.
func (s *missSpill) close() {
	_ = s.f.Close()
	if s.path != "" {
		_ = os.Remove(s.path)
	}
}
