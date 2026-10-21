package learn

import (
	"encoding/base64"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// The reference: Classify, appeared, sources, unaccounted and ReadBack as
// they were before the served index, which read every leaf served before a
// request for each value it drifted. unaccounted's why names the first word
// of what changed, in no fixed order, that appears earlier: refUnaccounted
// gives every why it can give.

func refClassify(dc *DriftCache, drift []string, live, rec Shape, at time.Time, earlier []Earlier, raw []Raw, resp []Leaf) ([]Learned, []map[string]bool) {
	if dc == nil {
		dc = NewDriftCache()
	}
	served := append([]Earlier(nil), earlier...)
	sort.SliceStable(served, func(i, j int) bool { return served[i].At.Before(served[j].At) })
	out := make([]Learned, 0, len(drift))
	whys := make([]map[string]bool, 0, len(drift))
	for _, p := range drift {
		r, l := rec.Leaves[p], live.Leaves[p]
		ln := Learned{Kind: KindNoise, Recorded: r, Live: l}
		var why map[string]bool
		if r.Value != "" {
			if from, ok := refAppeared(dc, r.Value, l.Value, at, served, raw); ok {
				ln.Kind, ln.From = KindVariable, from
			} else if echo := Echoes(dc, r, resp); len(echo) > 0 {
				ln.Kind, ln.Echo = KindEcho, echo
			}
		}
		if ln.Kind == KindNoise || ln.Kind == KindEcho {
			if set := refUnaccounted(dc, dc.of(r.Value, l.Value), at, served, raw); len(set) > 0 {
				ln.Kind, ln.Echo, why = KindUnaccounted, nil, set
			}
		}
		if ln.Kind == KindNoise {
			if w := echoedPart(dc, dc.of(r.Value, l.Value), resp); w != "" {
				ln.Kind, why = KindUnaccounted, map[string]bool{w: true}
			}
		}
		out = append(out, ln)
		whys = append(whys, why)
	}
	return out, whys
}

func refSources(e, v *valueDrift) (found, whole bool) {
	if e.rec == v.rec {
		return true, true
	}
	for w := range v.wholes {
		if e.wholes[w] {
			return true, false
		}
	}
	if e.clock.whole && v.b64Holds(e) || v.clock.whole && e.b64Holds(v) {
		return true, false
	}
	for i := range readings {
		lost, gained := false, false
		for t := range v.lost[i] {
			if e.lost[i][t] && !clockWord(t, true, v, v.clock, e.clock) {
				lost = true
				break
			}
		}
		for t := range v.gained[i] {
			if e.gained[i][t] && !clockWord(t, false, v, v.clock, e.clock) {
				gained = true
				break
			}
		}
		if lost && gained {
			return true, false
		}
	}
	drifted := func(p run, rec, live string) bool {
		return ContainsToken(rec, p.rec) && ContainsToken(live, p.live) && !ContainsToken(live, p.rec) && !ContainsToken(rec, p.live)
	}
	for _, p := range v.pieces {
		if !isClock(p, v.clock, e.clock) && (drifted(p, e.rec, e.live) || e.decoded && drifted(p, e.decRec, e.decLive)) {
			return true, false
		}
	}
	for _, p := range e.pieces {
		if !isClock(p, e.clock, v.clock) && (drifted(p, v.rec, v.live) || v.decoded && drifted(p, v.decRec, v.decLive)) {
			return true, false
		}
	}
	return false, false
}

// refAppeared is appeared of served sorted by when each was recorded.
func refAppeared(dc *DriftCache, recorded, now string, at time.Time, served []Earlier, raw []Raw) (Place, bool) {
	v := dc.of(recorded, now)
	var best Place
	rank := 7
	for _, e := range served {
		for _, s := range e.Seen {
			if s.Live == s.Leaf.Value {
				continue
			}
			ok, whole := refSources(dc.of(s.Leaf.Value, s.Live), v)
			if !ok {
				continue
			}
			r := 6
			if s.Side == Request {
				switch {
				case e.Own:
					r = 0
				case e.Run:
					r = 1
				default:
					r = 2
				}
				if !whole {
					r += 3
				}
			}
			if r < rank {
				best, rank = Place{Mock: e.Mock, Side: s.Side, Path: s.Leaf.Path, Noise: s.Leaf.Noise,
					Var: s.Place, Live: s.Live, Whole: whole, Own: e.Own, Run: e.Run, Name: s.Name}, r
			}
			if rank == 0 {
				return best, true
			}
		}
	}
	if rank < 7 {
		return best, true
	}
	var searched []string
	if !v.clock.whole {
		searched = append(searched, recorded)
	}
	add := func(p run) {
		if p.rec != recorded && !isClock(p, v.clock) {
			ws, _ := v.asWords(p.rec, true)
			searched = append(searched, ws...)
		}
	}
	for i := range readings {
		for t := range v.lost[i] {
			if t != recorded && !clockWord(t, true, v, v.clock) {
				ws, _ := v.asWords(t, true)
				searched = append(searched, ws...)
			}
		}
	}
	for _, p := range v.pieces {
		add(p)
	}
	for _, x := range raw {
		if !x.Served && !x.At.Before(at) {
			continue
		}
		texts := []string{x.Text, dc.rawExtra(x.Text)}
		for _, text := range texts {
			if v.clock.whole && (containsWholeClock(text, recorded) || v.decoded && containsWholeClock(text, v.decRec)) {
				return Place{Mock: x.Mock}, true
			}
		}
		for _, w := range searched {
			if ContainsToken(texts[0], w) || ContainsToken(texts[1], w) {
				return Place{Mock: x.Mock}, true
			}
		}
	}
	return Place{}, false
}

// refUnaccounted is every why unaccounted of served sorted by when each was
// recorded can give: its words are read in no fixed order within each kind.
func refUnaccounted(dc *DriftCache, v *valueDrift, at time.Time, served []Earlier, raw []Raw) map[string]bool {
	if v.unparsed != "" {
		return map[string]bool{v.unparsed: true}
	}
	check := func(i int, t string, recorded, moved bool) string {
		if moved && allClock(t, recorded, v.clock) || !moved && clockWord(t, recorded, v, v.clock) {
			return ""
		}
		us, inUUID := v.asWords(t, recorded)
		holds := func(ed *valueDrift) bool {
			if !inUUID {
				return recorded && ed.recTok[i][t] || !recorded && ed.liveTok[i][t]
			}
			for _, u := range us {
				if recorded && ed.uuidRec[u] || !recorded && ed.uuidLive[u] {
					return true
				}
			}
			return false
		}
		for _, e := range served {
			where := "earlier in the test"
			if e.Run {
				where = "in the test set's start-up"
			}
			for _, s := range e.Seen {
				ed := dc.of(s.Leaf.Value, s.Live)
				if holds(ed) {
					return "a word of it appears " + where + ", in " + e.Mock + " at " + s.Leaf.Path + ", which did not drift with it: keploy cannot tell whether it came from there"
				}
			}
		}
		for _, x := range raw {
			if !x.Served && !x.At.Before(at) {
				continue
			}
			found := !inUUID && dc.rawTokens(x.Text)[i][t]
			for _, u := range us {
				found = found || inUUID && dc.rawUUIDs(x.Text)[u]
			}
			if found {
				return "a word of it appears earlier in the test, in " + x.Mock + "'s payload: keploy cannot tell whether it came from there"
			}
		}
		return ""
	}
	for i := range readings {
		for _, k := range []struct {
			words           map[string]bool
			recorded, moved bool
		}{{v.lost[i], true, false}, {v.gained[i], false, false}, {v.movedRec[i], true, true}, {v.movedLive[i], false, true}} {
			out := map[string]bool{}
			for t := range k.words {
				if why := check(i, t, k.recorded, k.moved); why != "" {
					out[why] = true
				}
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	return nil
}

func refReadBack(r Leaf, record string, earlier []Earlier) (Place, bool) {
	if r.Unnamed || r.Value == "" || record == "" {
		return Place{}, false
	}
	served := append([]Earlier(nil), earlier...)
	sort.SliceStable(served, func(i, j int) bool { return served[i].At.Before(served[j].At) })
	var first Place
	found := false
	live, wrote := "", false
	for _, e := range served {
		for _, s := range e.Seen {
			if s.Side != Request || !s.Written || s.Record != record || s.Leaf.Value != r.Value {
				continue
			}
			if !wrote {
				live, wrote = s.Live, true
			} else if s.Live != live {
				return Place{}, false
			}
			if s.Place == "" || found {
				continue
			}
			first = Place{Mock: e.Mock, Side: Request, Path: s.Leaf.Path, Noise: s.Leaf.Noise,
				Var: s.Place, Live: s.Live, Whole: true, Own: e.Own, Run: e.Run, Name: s.Name}
			found = true
		}
	}
	if !found || live == r.Value || live == "" {
		return Place{}, false
	}
	return first, true
}

// servedGen makes the exchanges of a test of Mongo- and HTTP-like calls:
// ids that flow from call to call (ObjectIDs, UUIDs, codes), whole and inside
// URLs, filters, text, escapes and base64 state; clock reads the lead moved
// and fresh ids, names and counters; values that did not drift.
type servedGen struct {
	r    *rand.Rand
	t0   time.Time
	ents []struct{ rec, live string }
	// payloads are the mocks of other kinds the test served, as the cache
	// reads each once (DriftCache.RawOf).
	payloads []Raw
}

func newServedGen(seed int64) *servedGen {
	return &servedGen{r: rand.New(rand.NewSource(seed)), t0: time.Date(2026, 10, 14, 10, 0, 41, 0, time.UTC)}
}

func (g *servedGen) hex(n int) string {
	const digits = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = digits[g.r.Intn(16)]
	}
	return string(b)
}

func (g *servedGen) code(n int) string {
	const alpha = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = alpha[g.r.Intn(len(alpha))]
	}
	return string(b)
}

func (g *servedGen) id() string {
	switch g.r.Intn(5) {
	case 0, 1:
		return g.hex(24)
	case 2:
		h := g.hex(32)
		return h[:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:]
	case 3:
		return g.code(8)
	}
	return strconv.Itoa(1000 + g.r.Intn(9000))
}

func (g *servedGen) pick(of []string) string { return of[g.r.Intn(len(of))] }

var genNames = []string{"Alice Smith", "Bob Jones", "Chen Wei", "Dana Ruiz", "Eve Kim", "Farah Ali"}

// genRecords are the records a write stores its values in (Seen.Record), ""
// for one the adapter names none of.
var genRecords = []string{"orders\x00o-1", "orders\x00o-2", ""}

// ent is an id of the test: one already sent, mostly, or a new one.
func (g *servedGen) ent() struct{ rec, live string } {
	if len(g.ents) > 0 && g.r.Intn(10) < 7 {
		return g.ents[g.r.Intn(len(g.ents))]
	}
	e := struct{ rec, live string }{g.id(), g.id()}
	g.ents = append(g.ents, e)
	return e
}

func (g *servedGen) value() (string, string) {
	e := g.ent()
	at := g.t0.Add(time.Duration(g.r.Intn(12)) * time.Second)
	f := at.Add(FullRoundLead)
	state := func(id string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(`{"ref":"` + id + `","n":1}`))
	}
	jwt := func(id string, iat time.Time) string {
		return "eyJhbGciOiJIUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"`+id+`","iat":`+strconv.FormatInt(iat.Unix(), 10)+`}`)) + "." + g.code(20)
	}
	jp := [][2]string{{"太郎", "花子"}, {"次郎", "三郎"}}[g.r.Intn(2)]
	switch g.r.Intn(27) {
	case 0, 1, 2:
		return e.rec, e.live
	case 3:
		return "https://api.example/v1/orders/" + e.rec + "?expand=items", "https://api.example/v1/orders/" + e.live + "?expand=items"
	case 4:
		n := g.pick(genNames)
		return "Order " + e.rec + " created for " + n, "Order " + e.live + " created for " + n
	case 5:
		return `{"_id":{"$oid":"` + e.rec + `"}}`, `{"_id":{"$oid":"` + e.live + `"}}`
	case 6:
		return "uploads%2F" + e.rec + ".png", "uploads%2F" + e.live + ".png"
	case 7:
		return state(e.rec), state(e.live)
	case 8:
		return at.Format(time.RFC3339), f.Format(time.RFC3339)
	case 9:
		return strconv.FormatInt(at.UnixMilli(), 10), strconv.FormatInt(f.UnixMilli()+int64(g.r.Intn(5)), 10)
	case 10:
		return g.hex(24), g.hex(24)
	case 11:
		c := g.pick([]string{"active", "true", "pending", "2", "en-US"})
		return c, c
	case 12:
		return g.pick(genNames), g.pick(genNames)
	case 13:
		o := g.ent()
		return e.rec + ":" + o.rec, e.live + ":" + o.live
	case 14:
		return uuid7(at, g.hex(18)), uuid7(f.Add(time.Duration(g.r.Intn(3))*time.Millisecond), g.hex(18))
	case 15:
		return "订单" + e.rec + "已发货", "订单" + e.live + "已发货"
	case 16:
		return "名前:" + g.pick([]string{"太郎", "花子", "次郎"}), "名前:" + g.pick([]string{"太郎", "花子", "次郎"})
	case 17:
		return strings.ToUpper(e.rec), strings.ToUpper(e.live)
	case 18:
		return jwt(e.rec, at), jwt(e.live, f)
	case 19:
		return strconv.FormatInt(at.Unix(), 10), strconv.FormatInt(f.Unix(), 10)
	case 20:
		return at.Format(time.RFC1123), f.Format(time.RFC1123)
	case 21:
		k := g.r.Intn(9)
		return fmt.Sprintf("seq-%d", k), fmt.Sprintf("seq-%d", k+1)
	case 22:
		return at.Format(time.RFC3339), at.Add(5 * time.Minute).Format(time.RFC3339)
	case 23:
		// A page of ids that moved on by one: the id that stayed is a piece
		// of it that drifted, no word it lost.
		o := g.ent()
		return "ids=" + e.rec + "," + o.rec, "ids=" + o.rec + "," + o.live
	case 24:
		return "名前:" + jp[0], "名前:" + jp[1]
	case 25:
		return "山田" + jp[0], "山田" + jp[1]
	}
	return e.rec, ""
}

// exchange is the k-th exchange served, of up to leaves leaves, of one of
// mocks mocks.
func (g *servedGen) exchange(k, leaves, mocks int) Earlier {
	e := Earlier{Mock: fmt.Sprintf("mock-%d", g.r.Intn(mocks)), At: g.t0.Add(time.Duration(k+g.r.Intn(5)-2) * time.Second), Own: g.r.Intn(3) > 0, Run: g.r.Intn(6) == 0}
	for i := 0; i < 1+g.r.Intn(leaves); i++ {
		rec, live := g.value()
		if g.r.Intn(8) == 0 {
			live = rec
		}
		s := Seen{Side: Request, Leaf: Leaf{Path: fmt.Sprintf("body.doc.f%d", i), Noise: fmt.Sprintf("body.doc.f%d", i), Type: String, Value: rec}, Live: live, Written: g.r.Intn(2) == 0}
		if s.Written {
			s.Record = g.pick(genRecords)
		}
		if g.r.Intn(3) == 0 {
			s.Side = Response
		}
		if g.r.Intn(3) > 0 {
			s.Place = s.Leaf.Path
		}
		if g.r.Intn(5) == 0 {
			s.Name = "v" + strconv.Itoa(g.r.Intn(4))
		}
		e.Seen = append(e.Seen, s)
	}
	return e
}

// call is a request paired with a mock recorded at at: its drifted leaves,
// its recorded answer, and the payloads of other mocks served or recorded
// before it.
type genCall struct {
	drift     []string
	live, rec Shape
	at        time.Time
	raw       []Raw
	resp      []Leaf
}

func (g *servedGen) call(k int) genCall {
	c := genCall{live: Shape{Leaves: map[string]Leaf{}}, rec: Shape{Leaves: map[string]Leaf{}}, at: g.t0.Add(time.Duration(k) * time.Second)}
	for i := 0; i < 2+g.r.Intn(6); i++ {
		p := fmt.Sprintf("body.q.f%d", i)
		rec, live := g.value()
		if live == rec {
			live += "x"
		}
		c.drift = append(c.drift, p)
		c.rec.Leaves[p] = Leaf{Path: p, Noise: p, Type: String, Value: rec}
		c.live.Leaves[p] = Leaf{Path: p, Noise: p, Type: String, Value: live}
	}
	if g.r.Intn(3) == 0 {
		for i := 0; i < g.r.Intn(4); i++ {
			e := g.ent()
			c.raw = append(c.raw, Raw{Mock: fmt.Sprintf("pg-%d", i), At: g.t0.Add(time.Duration(g.r.Intn(k+2)) * time.Second), Served: g.r.Intn(2) == 0,
				Text: "INSERT INTO t (ref, note) VALUES ('" + e.rec + "', '" + g.pick(genNames) + "')\n"})
		}
		return c
	}
	// Payloads the cache read: new ones, and those served before, in any
	// order, one of them twice.
	for i := 0; i < 1+g.r.Intn(3); i++ {
		text := ""
		for f := 0; f < 1+g.r.Intn(4); f++ {
			rec, live := g.value()
			text += fmt.Sprintf("f%d\n%s\n", f, g.pick([]string{rec, live}))
		}
		g.payloads = append(g.payloads, Raw{Mock: fmt.Sprintf("pg-%d", len(g.payloads)), At: g.t0.Add(time.Duration(g.r.Intn(k+2)) * time.Second), Text: text, read: &rawRead{text: text}})
	}
	for _, i := range g.r.Perm(len(g.payloads)) {
		if g.r.Intn(3) > 0 {
			x := g.payloads[i]
			x.Served = g.r.Intn(2) == 0
			c.raw = append(c.raw, x)
		}
	}
	if len(c.raw) > 0 {
		c.raw = append(c.raw, c.raw[g.r.Intn(len(c.raw))])
	}
	for i := 0; i < g.r.Intn(4); i++ {
		rec, _ := g.value()
		c.resp = append(c.resp, Leaf{Path: fmt.Sprintf("body.r%d", i), Type: String, Value: rec})
	}
	return c
}

// sameLearned fails t where got is not what the reference learned: the same
// kinds, places, echoes, and a why the reference can give.
func sameLearned(t *testing.T, what string, got, want []Learned, whys []map[string]bool) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d learned, want %d", what, len(got), len(want))
	}
	for i := range got {
		g, w := got[i], want[i]
		why := g.Why
		g.Why, w.Why = "", ""
		if !reflect.DeepEqual(g, w) {
			t.Fatalf("%s: leaf %s learned\n %+v\nwant\n %+v", what, got[i].Recorded.Path, got[i], want[i])
		}
		if whys[i] == nil && why != "" || whys[i] != nil && !whys[i][why] {
			t.Fatalf("%s: leaf %s refused with %q, want one of %v", what, got[i].Recorded.Path, why, whys[i])
		}
	}
}

// Classify learns what the reference learns, of exchanges given in any
// order, as a list each request is given anew or one that grows.
func TestClassifyReadsTheServedIndexAsEveryLeaf(t *testing.T) {
	for seed := int64(1); seed <= 12; seed++ {
		g := newServedGen(seed)
		var earlier []Earlier
		grown, ref := NewDriftCache(), NewDriftCache()
		for k := 0; k < 120; k++ {
			earlier = append(earlier, g.exchange(k, 8, 1+k/3))
			if k%10 != 9 {
				continue
			}
			c := g.call(k)
			want, whys := refClassify(ref, c.drift, c.live, c.rec, c.at, earlier, c.raw, c.resp)
			sameLearned(t, fmt.Sprintf("seed %d, %d served, a list that grows", seed, len(earlier)), Classify(grown, c.drift, c.live, c.rec, c.at, earlier, c.raw, c.resp), want, whys)
			sameLearned(t, fmt.Sprintf("seed %d, %d served, a new cache", seed, len(earlier)), Classify(nil, c.drift, c.live, c.rec, c.at, earlier, c.raw, c.resp), want, whys)
			shuffled := append([]Earlier(nil), earlier...)
			g.r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
			want, whys = refClassify(ref, c.drift, c.live, c.rec, c.at, shuffled, c.raw, c.resp)
			sameLearned(t, fmt.Sprintf("seed %d, %d served, shuffled", seed, len(earlier)), Classify(grown, c.drift, c.live, c.rec, c.at, shuffled, c.raw, c.resp), want, whys)
		}
	}
}

// notOf is earlier without mock's exchanges, as the adapters read a
// request's earlier places (the paired mock's own left out).
func notOf(earlier []Earlier, mock string) []Earlier {
	var out []Earlier
	for _, e := range earlier {
		if e.Mock != mock {
			out = append(out, e)
		}
	}
	return out
}

// recordedBefore is earlier without the exchanges recorded after at, as the
// adapters read a request's earlier places (the Mongo parser's
// recordedBefore, the HTTP read-back's writtenBefore).
func recordedBefore(earlier []Earlier, at time.Time) []Earlier {
	var out []Earlier
	for _, e := range earlier {
		if !e.At.After(at) {
			out = append(out, e)
		}
	}
	return out
}

// Classify, with the tracker's cache, learns what the reference learns of
// what the tracker gave the request: the run's exchanges and the test's, the
// paired mock's left out, read when another call was served after it, or
// after the next test began; and a list the tracker never gave.
func TestClassifyReadsTheTrackersIndexAsEveryLeaf(t *testing.T) {
	for seed := int64(1); seed <= 10; seed++ {
		g := newServedGen(100 + seed)
		tr := NewTracker()
		for k := 0; k < 6; k++ {
			tr.Served(0, Named(g.exchange(k, 6, 4), nil))
		}
		ref := NewDriftCache()
		tr.Begin()
		var stale []Earlier
		for k := 6; k < 160; k++ {
			n, _, _, _ := tr.Test()
			tr.Served(n, Named(g.exchange(k, 8, 2+k/4), nil))
			if k == 100 {
				_, stale, _, _ = tr.Test()
				tr.End()
				tr.Begin()
			}
			if k%8 != 0 {
				continue
			}
			_, earlier, _, _ := tr.Test()
			c := g.call(k)
			lists := map[string][]Earlier{"all": earlier}
			if len(earlier) > 0 {
				lists["the paired mock's left out"] = notOf(earlier, earlier[g.r.Intn(len(earlier))].Mock)
				first := earlier[0]
				for _, e := range earlier {
					if e.At.Before(first.At) {
						first = e
					}
				}
				lists["the first call's mock left out"] = notOf(earlier, first.Mock)
				lists["an older read"] = notOf(earlier[:g.r.Intn(len(earlier))], "mock-1")
				lists["two mocks left out"] = notOf(notOf(earlier, "mock-0"), "mock-1")
				one := g.r.Intn(len(earlier))
				lists["one call left out"] = append(append([]Earlier(nil), earlier[:one]...), earlier[one+1:]...)
				lists["out of order"] = append(append([]Earlier(nil), earlier[len(earlier)/2:]...), earlier[:len(earlier)/2]...)
				cut := earlier[g.r.Intn(len(earlier))].At
				lists["recorded before a call, its mock left out"] = recordedBefore(notOf(earlier, earlier[g.r.Intn(len(earlier))].Mock), cut)
				lists["recorded before the paired call"] = recordedBefore(earlier, c.at)
				lists["recorded before a call, an older read"] = recordedBefore(notOf(earlier[:g.r.Intn(len(earlier))], "mock-1"), cut)
				lists["recorded before a call, two mocks left out"] = recordedBefore(notOf(notOf(earlier, "mock-0"), "mock-1"), cut)
			}
			if stale != nil {
				lists["the previous test's"] = notOf(stale, "mock-2")
			}
			// What the tracker gave, the run's exchanges first, is read in
			// the test's own index: each exchange once, not anew.
			for _, name := range []string{"all", "the paired mock's left out"} {
				if list, ok := lists[name]; ok && tr.Drifts().view(list).ix != tr.Drifts().ix {
					t.Fatalf("seed %d, call %d, %s: a list the tracker gave is read on its own", seed, k, name)
				}
			}
			names := make([]string, 0, len(lists))
			for name := range lists {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				list := lists[name]
				want, whys := refClassify(ref, c.drift, c.live, c.rec, c.at, list, c.raw, c.resp)
				sameLearned(t, fmt.Sprintf("seed %d, call %d, %s (%d served)", seed, k, name, len(list)), Classify(tr.Drifts(), c.drift, c.live, c.rec, c.at, list, c.raw, c.resp), want, whys)
				for _, e := range list {
					for _, s := range e.Seen {
						r := Leaf{Path: "body.r", Type: String, Value: s.Leaf.Value}
						for _, record := range genRecords {
							got, ok := ReadBack(r, record, list)
							want, wok := refReadBack(r, record, list)
							if got != want || ok != wok {
								t.Fatalf("seed %d, call %d, %s: %q of record %q read back from %+v (%v), want %+v (%v)", seed, k, name, r.Value, record, got, ok, want, wok)
							}
						}
					}
				}
			}
		}
	}
}

// The index finds every earlier value sources does, and the first leaf
// that holds each word, in a list that leaves a mock out or ends early.
func TestTheServedIndexFindsEverySourceAndWord(t *testing.T) {
	g := newServedGen(7)
	tr := NewTracker()
	tr.Begin()
	n, _, _, _ := tr.Test()
	for k := 0; k < 300; k++ {
		tr.Served(n, Named(g.exchange(k, 8, 40), nil))
	}
	_, all, _, _ := tr.Test()
	dc := tr.Drifts()
	rules := map[string]int{}
	for trial := 0; trial < 40; trial++ {
		earlier := notOf(all[:g.r.Intn(len(all)+1)], fmt.Sprintf("mock-%d", g.r.Intn(40)))
		if trial%2 == 1 {
			// As the Mongo parser reads them: those recorded after the
			// paired call left out too, of many mocks, recorded in another
			// order than served.
			earlier = recordedBefore(earlier, all[g.r.Intn(len(all))].At)
		}
		w := dc.view(earlier)
		if w.ix != dc.ix {
			t.Fatalf("trial %d: a list the tracker gave is read on its own", trial)
		}
		served := append([]Earlier(nil), earlier...)
		sort.SliceStable(served, func(i, j int) bool { return served[i].At.Before(served[j].At) })
		for q := 0; q < 10; q++ {
			v := dc.of(g.value())
			w.ix.mu.Lock()
			found := map[*valueDrift]bool{}
			for _, id := range w.candidates(v) {
				found[w.ix.pairs[id].d] = true
			}
			w.ix.mu.Unlock()
			for _, e := range served {
				for _, s := range e.Seen {
					if s.Live == s.Leaf.Value {
						continue
					}
					d := dc.of(s.Leaf.Value, s.Live)
					for _, rule := range sourceRules(d, v) {
						rules[rule]++
						if !found[d] {
							t.Fatalf("trial %d: %q→%q comes from %q→%q by %s, which the index does not find", trial, v.rec, v.live, d.rec, d.live, rule)
						}
					}
				}
			}
			for i := range readings {
				for _, words := range []map[string]bool{v.recTok[i], v.liveTok[i]} {
					for word := range words {
						for _, recorded := range []bool{true, false} {
							mock, path, run, ok := w.holding(i, word, recorded)
							wmock, wpath, wrun, wok := "", "", false, false
						scan:
							for _, e := range served {
								for _, s := range e.Seen {
									if d := dc.of(s.Leaf.Value, s.Live); recorded && d.recTok[i][word] || !recorded && d.liveTok[i][word] {
										wmock, wpath, wrun, wok = e.Mock, s.Leaf.Path, e.Run, true
										break scan
									}
								}
							}
							if mock != wmock || path != wpath || run != wrun || ok != wok {
								t.Fatalf("trial %d: %q (reading %d, recorded %v) is first held at %s %s (run %v, %v), want %s %s (run %v, %v)", trial, word, i, recorded, mock, path, run, ok, wmock, wpath, wrun, wok)
							}
						}
					}
				}
			}
		}
	}
	if shuffled := append([]Earlier{all[len(all)-1]}, all[:len(all)-1]...); dc.view(shuffled).ix == dc.ix {
		t.Fatal("a list in another order than the tracker's is read in its index")
	}
	for _, rule := range []string{"exact", "wholes", "clock in base64", "base64 holds clock", "lost and gained", "piece of v", "wordless piece of v", "piece of e"} {
		if rules[rule] == 0 {
			t.Errorf("no value came from an earlier one by %s: the test reads nothing of it (%v)", rule, rules)
		}
	}
}

// sourceRules are the rules of sources by which e is where v came from,
// each read on its own.
func sourceRules(e, v *valueDrift) []string {
	var out []string
	if e.rec == v.rec {
		out = append(out, "exact")
	}
	for w := range v.wholes {
		if e.wholes[w] {
			out = append(out, "wholes")
			break
		}
	}
	if e.clock.whole && v.b64Holds(e) {
		out = append(out, "clock in base64")
	}
	if v.clock.whole && e.b64Holds(v) {
		out = append(out, "base64 holds clock")
	}
	for i := range readings {
		lost, gained := false, false
		for t := range v.lost[i] {
			lost = lost || e.lost[i][t] && !clockWord(t, true, v, v.clock, e.clock)
		}
		for t := range v.gained[i] {
			gained = gained || e.gained[i][t] && !clockWord(t, false, v, v.clock, e.clock)
		}
		if lost && gained {
			out = append(out, "lost and gained")
			break
		}
	}
	for _, p := range v.pieces {
		if !isClock(p, v.clock, e.clock) && (codrifted(p, e.rec, e.live) || e.decoded && codrifted(p, e.decRec, e.decLive)) {
			if longestWord(p.rec) == "" && longestWord(p.live) == "" {
				out = append(out, "wordless piece of v")
			} else {
				out = append(out, "piece of v")
			}
		}
	}
	for _, p := range e.pieces {
		if !isClock(p, e.clock, v.clock) && (codrifted(p, v.rec, v.live) || v.decoded && codrifted(p, v.decRec, v.decLive)) {
			out = append(out, "piece of e")
			break
		}
	}
	return out
}

// Each rule of sources finds a value where it alone holds: the index keeps
// a key for each.
func TestTheServedIndexFindsEachRuleAlone(t *testing.T) {
	at := time.Date(2026, 10, 14, 10, 0, 41, 0, time.UTC)
	f := at.Add(FullRoundLead)
	for _, c := range []struct {
		name, rec, live, recorded, now string
	}{
		// Its pieces drifted only with the clock, and the value with
		// another amount: the same value whole, nothing else.
		{"one clock read whole, moved by another amount", at.Format(time.RFC3339), f.Format(time.RFC3339), at.Format(time.RFC3339), at.Add(5 * time.Minute).Format(time.RFC3339)},
		// The id that stayed on the page is a piece of it that drifted in
		// the earlier value, whose own pieces hold no word of it.
		{"a page of ids that moved on by one", "B7K29QXM-LONGERWORD1", "C3M81XPD-LONGERWORD2-x", "ids=A1B2C3D4,B7K29QXM", "ids=B7K29QXM,C3M81XPD"},
		{"an id of a page that moved on by one", "ids=A1B2C3D4,B7K29QXM", "ids=B7K29QXM,C3M81XPD", "B7K29QXM-LONGERWORD1", "C3M81XPD-LONGERWORD2-x"},
		{"a name in text of a script with no spaces", "山田太郎-LONGWORD1", "山田花子-LONGWORD2-x", "名前:太郎", "名前:花子"},
	} {
		e := Earlier{Mock: "mock-1", At: at, Own: true, Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "body.x", Type: String, Value: c.rec}, Live: c.live, Place: "body.x"}}}
		dc := NewDriftCache()
		want, wok := refAppeared(dc, c.recorded, c.now, at.Add(time.Minute), []Earlier{e}, nil)
		got, ok := appeared(dc, c.recorded, c.now, at.Add(time.Minute), []Earlier{e}, nil)
		if !wok || ok != wok || got != want {
			t.Errorf("%s: %q→%q comes from %+v (%v), want %+v (%v)", c.name, c.recorded, c.now, got, ok, want, wok)
		}
	}
	// The first place of a value is the paired mock's own, left out: the
	// next one, of another mock.
	tr := NewTracker()
	tr.Begin()
	n, _, _, _ := tr.Test()
	for i, mock := range []string{"mock-2", "mock-1", "mock-2", "mock-3"} {
		tr.Served(n, Named(Earlier{Mock: mock, At: at.Add(time.Duration(3-i) * time.Second), Own: true, Seen: []Seen{
			{Side: Request, Leaf: Leaf{Path: "body.id", Type: String, Value: "A1B2C3D4"}, Live: "Z9Y8X7W6", Place: "body.id"},
		}}, nil))
	}
	_, earlier, _, _ := tr.Test()
	for _, left := range []string{"mock-1", "mock-2", "mock-3"} {
		list := notOf(earlier, left)
		// The reference reads them sorted, as Classify gave them.
		sorted := append([]Earlier(nil), list...)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].At.Before(sorted[j].At) })
		got, ok := appeared(tr.Drifts(), "A1B2C3D4", "Z9Y8X7W6", at.Add(time.Minute), list, nil)
		want, wok := refAppeared(NewDriftCache(), "A1B2C3D4", "Z9Y8X7W6", at.Add(time.Minute), sorted, nil)
		why := unaccounted(tr.Drifts(), tr.Drifts().of("A1B2C3D4", "Q5R6S7T8"), at.Add(time.Minute), list, nil)
		ref := NewDriftCache()
		wwhy := refUnaccounted(ref, ref.of("A1B2C3D4", "Q5R6S7T8"), at.Add(time.Minute), sorted, nil)
		if !ok || got != want || ok != wok || !wwhy[why] {
			t.Errorf("%s left out: the value comes from %+v (%v), want %+v (%v); refused with %q, want one of %v", left, got, ok, want, wok, why, wwhy)
		}
	}
}

// The cache reads each payload once, and a request still reads its payloads
// in the order given: the first that holds a word, or a value, is where it
// is.
func TestThePayloadsAreReadInTheirOrder(t *testing.T) {
	at := time.Date(2026, 10, 14, 10, 0, 42, 0, time.UTC)
	mock := func(name, data string) *models.Mock {
		return &models.Mock{Name: name, Kind: models.GENERIC, Spec: models.MockSpec{ReqTimestampMock: at.Add(-time.Second),
			GenericRequests: []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "string", Data: data}}}}}}
	}
	a, b, c := mock("raw-a", "INSERT INTO t (id) VALUES ('A7K29QXM')"), mock("raw-b", "SET order:A7K29QXM 1"), mock("raw-c", "PING")
	dc := NewDriftCache()
	for _, order := range [][]*models.Mock{{c, a, b, a}, {b, c, a, b}, {c, b, a}} {
		var raw []Raw
		for _, m := range order {
			raw = append(raw, dc.RawOf(m, true))
		}
		got, ok := appeared(dc, "A7K29QXM", "Z3M81XPD", at, nil, raw)
		want, wok := refAppeared(NewDriftCache(), "A7K29QXM", "Z3M81XPD", at, nil, raw)
		if !ok || got != want || ok != wok {
			t.Errorf("%s first: the value comes from %+v (%v), want %+v (%v)", order[0].Name, got, ok, want, wok)
		}
		ref := NewDriftCache()
		why := unaccounted(dc, dc.of("ord-A7K29QXM", "ord-Z3M81XPD"), at, nil, raw)
		if wwhy := refUnaccounted(ref, ref.of("ord-A7K29QXM", "ord-Z3M81XPD"), at, nil, raw); !wwhy[why] {
			t.Errorf("%s first: refused with %q, want one of %v", order[0].Name, why, wwhy)
		}
	}
}

// ReadBack reads the place written first, and of two written at one time
// the one served first, as the reference does: a write at no place a
// variable can hold, and one into another record, are not that place.
func TestReadBackTakesTheFirstOfOneTime(t *testing.T) {
	t0 := time.Unix(1_792_000_000, 0)
	const order = "orders\x00o-1"
	noPlace := wrote("mock-5", t0.Add(-2*time.Second), order, "body.blob", String, "BJBM-NZBS", "FRST-XKCD")
	noPlace.Seen[0].Place = ""
	earlier := []Earlier{
		wrote("mock-4", t0.Add(time.Second), order, "body.code", String, "BJBM-NZBS", "FRST-XKCD"),
		wrote("mock-3", t0, order, "body.code", String, "BJBM-NZBS", "FRST-XKCD"),
		wrote("mock-1", t0, order, "body.code", String, "BJBM-NZBS", "FRST-XKCD"),
		wrote("mock-2", t0.Add(-time.Second), "orders\x00o-2", "body.other", String, "BJBM-NZBS", "QQRT-XKCD"),
		noPlace,
	}
	r := Leaf{Path: "body.value.x", Type: String, Value: "BJBM-NZBS"}
	got, ok := ReadBack(r, order, earlier)
	want, wok := refReadBack(r, order, earlier)
	if !ok || got != want || ok != wok || got.Mock != "mock-3" {
		t.Fatalf("read back from %+v (%v), want %+v (%v)", got, ok, want, wok)
	}
}

// A payload whose answer the agent parked on disk, read back for the test
// (DriftCache.ReadRaw), is read in the test's cache as any other: once, and
// in the order given.
func TestAParkedPayloadIsReadInTheCache(t *testing.T) {
	at := time.Date(2026, 10, 14, 10, 0, 42, 0, time.UTC)
	parked := &models.Mock{Name: "raw-a", Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: at.Add(-time.Second), HTTPReq: &models.HTTPReq{Method: "GET", URL: "/orders"}}}
	parked.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) {
		return &models.HTTPResp{StatusCode: 200, Body: `{"id":"A7K29QXM"}`}, nil, nil
	})
	other := &models.Mock{Name: "raw-b", Kind: models.GENERIC, Spec: models.MockSpec{ReqTimestampMock: at.Add(-time.Second),
		GenericRequests: []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "string", Data: "SET order:A7K29QXM 1"}}}}}}
	dc := NewDriftCache()
	for _, order := range [][]*models.Mock{{parked, other}, {other, parked}} {
		var raw []Raw
		for _, m := range order {
			x, err := dc.ReadRaw(m, true)
			if err != nil {
				t.Fatal(err)
			}
			raw = append(raw, x)
		}
		if dc.rawView(raw, at).pos == nil {
			t.Fatalf("%s first: a parked payload is read in full for every word, not in the cache", order[0].Name)
		}
		got, ok := appeared(dc, "A7K29QXM", "Z3M81XPD", at, nil, raw)
		want, wok := refAppeared(NewDriftCache(), "A7K29QXM", "Z3M81XPD", at, nil, raw)
		if !ok || got != want || got.Mock != order[0].Name {
			t.Errorf("%s first: the value comes from %+v (%v), want %+v (%v)", order[0].Name, got, ok, want, wok)
		}
		ref := NewDriftCache()
		why := unaccounted(dc, dc.of("ord-A7K29QXM", "ord-Z3M81XPD"), at, nil, raw)
		if wwhy := refUnaccounted(ref, ref.of("ord-A7K29QXM", "ord-Z3M81XPD"), at, nil, raw); !wwhy[why] {
			t.Errorf("%s first: refused with %q, want one of %v", order[0].Name, why, wwhy)
		}
	}
}

// A late call of a long test reads the few leaves that can be its earlier
// places, not every leaf the test served before it: Classify of a test that
// served 500 calls of 20 leaves takes a small part of the time the reference
// reading, which reads every leaf for each drifted value, takes.
func TestClassifyOfALongTestReadsTheIndexNotEveryLeaf(t *testing.T) {
	if testing.Short() {
		t.Skip("times Classify against the reference reading")
	}
	tr, earlier, c, _ := benchServed(500, 20)
	dc, ref := tr.Drifts(), NewDriftCache()
	want, whys := refClassify(ref, c.drift, c.live, c.rec, c.at, earlier, c.raw, c.resp)
	sameLearned(t, "a test that served 500 calls", Classify(dc, c.drift, c.live, c.rec, c.at, earlier, c.raw, c.resp), want, whys)
	took := func(f func()) time.Duration {
		best := time.Duration(1<<63 - 1)
		for i := 0; i < 3; i++ {
			start := time.Now()
			f()
			if d := time.Since(start); d < best {
				best = d
			}
		}
		return best
	}
	indexed := took(func() { Classify(dc, c.drift, c.live, c.rec, c.at, earlier, c.raw, c.resp) })
	every := took(func() { refClassify(ref, c.drift, c.live, c.rec, c.at, earlier, c.raw, c.resp) })
	if indexed*20 > every {
		t.Fatalf("Classify took %v of a test that served 500 calls, the reference reading %v: it reads every leaf", indexed, every)
	}
	t.Logf("Classify %v, the reference reading %v", indexed, every)
}

// Requests of one test learn at once while the test is served, as each
// would alone.
func TestClassifyReadsTheServedIndexAtOnce(t *testing.T) {
	g := newServedGen(42)
	tr := NewTracker()
	tr.Begin()
	n, _, _, _ := tr.Test()
	type job struct {
		c       genCall
		earlier []Earlier
	}
	var jobs []job
	for k := 0; k < 80; k++ {
		tr.Served(n, Named(g.exchange(k, 6, 10), nil))
		if k%4 == 0 {
			_, earlier, _, _ := tr.Test()
			jobs = append(jobs, job{g.call(k), notOf(earlier, fmt.Sprintf("mock-%d", g.r.Intn(10)))})
		}
	}
	ref := NewDriftCache()
	want := make([][]Learned, len(jobs))
	whys := make([][]map[string]bool, len(jobs))
	for i, j := range jobs {
		want[i], whys[i] = refClassify(ref, j.c.drift, j.c.live, j.c.rec, j.c.at, j.earlier, j.c.raw, j.c.resp)
	}
	// A new test whose exchanges are served while its requests learn.
	tr.Begin()
	m, _, _, _ := tr.Test()
	var wg sync.WaitGroup
	got := make([][]Learned, len(jobs))
	for i := range jobs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			j := jobs[i]
			got[i] = Classify(tr.Drifts(), j.c.drift, j.c.live, j.c.rec, j.c.at, j.earlier, j.c.raw, j.c.resp)
		}(i)
	}
	for _, j := range jobs {
		for _, e := range j.earlier {
			wg.Add(1)
			go func(e Earlier) {
				defer wg.Done()
				tr.Served(m, e)
			}(e)
		}
	}
	wg.Wait()
	for i := range jobs {
		sameLearned(t, fmt.Sprintf("request %d", i), got[i], want[i], whys[i])
	}
}

// benchServed is a test that served n Mongo-like calls of leaves leaves each
// (the generation test's inserts and finds), and the call that learns next.
func benchServed(n, leaves int) (*Tracker, []Earlier, genCall, func() Earlier) {
	g := newServedGen(2026)
	tr := NewTracker()
	tr.Begin()
	test, _, _, _ := tr.Test()
	k := 0
	next := func() Earlier {
		e := g.exchange(k, 1, 200)
		if g.r.Intn(10) == 0 {
			// A mock recorded later in the recording, served early (the
			// pool's): what a call recorded before it reads leaves it out.
			e.At = e.At.Add(100000 * time.Second)
		}
		e.Seen = e.Seen[:0]
		for i := 0; i < leaves; i++ {
			rec, live := g.value()
			e.Seen = append(e.Seen, Seen{Side: Request, Leaf: Leaf{Path: fmt.Sprintf("body.documents[0].f%d", i), Type: String, Value: rec}, Live: live, Place: fmt.Sprintf("body.documents[0].f%d", i)})
		}
		k++
		return Named(e, nil)
	}
	for k < n {
		tr.Served(test, next())
	}
	_, earlier, _, _ := tr.Test()
	c := g.call(n)
	for i := len(c.drift); i < 10; i++ {
		p := fmt.Sprintf("body.q.f%d", i)
		rec, live := g.value()
		if live == rec {
			live += "x"
		}
		c.drift = append(c.drift, p)
		c.rec.Leaves[p] = Leaf{Path: p, Noise: p, Type: String, Value: rec}
		c.live.Leaves[p] = Leaf{Path: p, Noise: p, Type: String, Value: live}
	}
	return tr, notOf(earlier, "mock-0"), c, next
}

// One insert's learning, of a test that served 500 and 5,000 calls of 20
// leaves: with the index, and as every leaf was read before it (reference).
func BenchmarkClassifyServed(b *testing.B) {
	for _, n := range []int{500, 5000} {
		tr, earlier, c, next := benchServed(n, 20)
		b.Run(fmt.Sprintf("indexed/%d", n), func(b *testing.B) {
			dc := tr.Drifts()
			Classify(dc, c.drift, c.live, c.rec, c.at, earlier, c.raw, c.resp)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				Classify(dc, c.drift, c.live, c.rec, c.at, earlier, c.raw, c.resp)
			}
		})
		b.Run(fmt.Sprintf("indexed-serving/%d", n), func(b *testing.B) {
			// One more call served before each insert learns, as in a test.
			more := make([]Earlier, b.N)
			for i := range more {
				more[i] = next()
			}
			dc := tr.Drifts()
			test, _, _, _ := tr.Test()
			Classify(dc, c.drift, c.live, c.rec, c.at, earlier, c.raw, c.resp)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				tr.Served(test, more[i])
				_, all, _, _ := tr.Test()
				Classify(dc, c.drift, c.live, c.rec, c.at, notOf(all, "mock-0"), c.raw, c.resp)
			}
		})
		b.Run(fmt.Sprintf("indexed-serving-recorded-before/%d", n), func(b *testing.B) {
			// As the Mongo parser reads each call's earlier places: its own
			// mock's left out, and those recorded after it.
			more := make([]Earlier, b.N)
			for i := range more {
				more[i] = next()
			}
			dc := tr.Drifts()
			test, _, _, _ := tr.Test()
			Classify(dc, c.drift, c.live, c.rec, c.at, earlier, c.raw, c.resp)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				tr.Served(test, more[i])
				_, all, _, _ := tr.Test()
				// Recorded just before the newest calls: those recorded
				// after it, of many mocks, are left out too.
				cut := c.at.Add(time.Duration(i-2-i%20) * time.Second)
				Classify(dc, c.drift, c.live, c.rec, c.at, recordedBefore(notOf(all, all[i%len(all)].Mock), cut), c.raw, c.resp)
			}
		})
		b.Run(fmt.Sprintf("reference/%d", n), func(b *testing.B) {
			dc := NewDriftCache()
			refClassify(dc, c.drift, c.live, c.rec, c.at, earlier, c.raw, c.resp)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				refClassify(dc, c.drift, c.live, c.rec, c.at, earlier, c.raw, c.resp)
			}
		})
	}
}

// One insert's learning, with the payloads of 500 and 5,000 mocks of other
// kinds served before it (an HTTP call's): read through the test's cache,
// and as every payload was read for each word (plain).
func BenchmarkClassifyPayloads(b *testing.B) {
	for _, n := range []int{500, 5000} {
		tr, earlier, c, _ := benchServed(200, 20)
		g := newServedGen(9)
		var read, plain []Raw
		for i := 0; i < n; i++ {
			text := ""
			for k := 0; k < 15; k++ {
				rec, _ := g.value()
				text += fmt.Sprintf("f%d\n%s\n", k, rec)
			}
			x := Raw{Mock: fmt.Sprintf("pg-%d", i), At: g.t0.Add(time.Duration(i) * time.Millisecond), Served: true, Text: text}
			plain = append(plain, x)
			x.read = &rawRead{text: text}
			read = append(read, x)
		}
		for _, list := range []struct {
			name string
			raw  []Raw
		}{{"read", read}, {"plain", plain}} {
			b.Run(fmt.Sprintf("%s/%d", list.name, n), func(b *testing.B) {
				dc := tr.Drifts()
				Classify(dc, c.drift, c.live, c.rec, c.at, earlier, list.raw, c.resp)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					Classify(dc, c.drift, c.live, c.rec, c.at, earlier, list.raw, c.resp)
				}
			})
		}
	}
}
