package learn

// A field keeps one learned role across its sibling places. A sibling is a
// recording of the same call as the one being classified: the same
// statement, on the same connection or in one recorded scope (one test, or
// one gap between tests, the app's start-up being one), which its
// protocol's learn step tells (Raw.Sibling). Where the sibling's learn step
// learned a place as noise, as an echo, or as a plain read of the
// application's clock, and the call holds an equal value at the same place,
// the value is learned the same way, never as a value carried from the
// sibling.
//
// An application that reads its clock once per statement in a coarser unit
// than its statements run apart in (Sequelize's JavaScript Date, to the
// millisecond: three role INSERTs made in one millisecond at start-up, sent
// at once on the pool's connections) writes one stamp at the same place of
// each.
// Read as a copy, the later statement's stamp was carried from the earlier
// one, and a replay passed or failed by whether its two stamps fell in one
// millisecond: the verdict hung on the clock's unit. Read in the sibling's
// role, it is the same at any unit.
//
// It fails closed:
//   - a sibling's place learned as anything else (a value carried from an
//     earlier place, a refusal) sets nothing aside, and the value is
//     followed or refused as before;
//   - so does a place whose recorded value is not the call's (Value): the
//     sibling holds the call's value elsewhere, which may be a copy;
//   - so do siblings that learned the place two ways (one role per field);
//   - the value is read again with every such sibling set aside, and the
//     reading is taken only where it learns the sibling's role: a reading
//     that finds another source, or refuses, or learns another kind, leaves
//     the value carried from the sibling, as before.
//
// A sibling record's full round had not served before the call has not been
// learned yet: the application sent its statements at once, on two pooled
// connections or more, and F took them in another order than the recording
// shows. Under the rule a copy follows its source (ClassifyFollowing) such a
// place is no source of the value (follows.notServed): in F the application
// sent the call first. An echo is refused beside a source the search set
// aside; beside a sibling's place set aside so, it stands, where every place
// the search went past is such a sibling's, holding the call's recorded
// value at the call's own leaf, its learn step has stated it unread there or
// learned it as the same echo, and no word was let go nor leaf set aside
// on the way (pastSiblingsOnly). Otherwise the echo is refused as before.
// Without it, the verdict hung on which connection F's pool sent first.

// SiblingRole is what a sibling's learn step learned one of its places as
// (Raw.Sibling): its kind, and the value the sibling recorded there. Kind ""
// states the place unread: the sibling's learn step has read none of its
// places yet, and every place of it that holds Value is one the call holds
// its own recorded value at too.
type SiblingRole struct {
	Kind  Kind
	Value string
}

// Sibling is r, a recording of the same call as the one being classified
// (its protocol's learn step tells: the same statement, on the same
// connection or in one recorded scope), stating the roles its learn step learned
// at its places (roles: by the leaf path the call names each place by, as a
// statement's binds are named alike in each of its recordings). A role is
// stated only where every place of r that holds its Value is one of that
// role's own: its place, the places an echo answers or uses, so that setting
// r aside sets aside no other place the value stands in.
func (r Raw) Sibling(roles map[string]SiblingRole) Raw {
	r.roles = roles
	return r
}

// keepsRole reports whether k is a role a sibling's place passes on to an
// equal value at the same place of the call: noise, an echo, or a plain
// read of the application's clock. A value carried from an earlier place, a
// refusal, or a read plus a constant offset (KindClockOffset) is none: such
// a sibling's place stays a source, and the value carried from it is
// followed or refused as before.
func keepsRole(k Kind) bool {
	switch k {
	case KindNoise, KindEcho, KindClockRead:
		return true
	}
	return false
}

// siblingsAside is raw without the siblings that learned the place r names
// in a role they pass on (keepsRole) with r's recorded value there, and that
// role; ok is false where none of them is the recording from names (the
// place the value was found carried from), or where they learned the place
// two ways.
func siblingsAside(raw []Raw, r Leaf, from Place) (own []Raw, role Kind, ok bool) {
	aside := func(x Raw) bool {
		sr := x.roles[r.Path]
		return keepsRole(sr.Kind) && sr.Value == r.Value
	}
	found := false
	for _, x := range raw {
		if !aside(x) {
			continue
		}
		if k := x.roles[r.Path].Kind; role != "" && k != role {
			return nil, "", false
		}
		role = x.roles[r.Path].Kind
		found = found || x.mock == from.Mock
	}
	if !found {
		return nil, "", false
	}
	own = make([]Raw, 0, len(raw))
	for _, x := range raw {
		if !aside(x) {
			own = append(own, x)
		}
	}
	return own, role, true
}

// pastSiblingsOnly reports whether every place the search for the value of
// r went past (refutedOf: classifyRead asks only where it went past one) is
// a sibling's that record's full round had not served before the call
// (follows.notServed), holding r's recorded value at r's own leaf, stated
// there unread or learned as kind; and that the search let no word go
// (first) and set no leaf aside (aside).
func (f *follows) pastSiblingsOnly(raw []Raw, r Leaf, kind Kind) bool {
	if f.first != "" || f.aside != "" {
		return false
	}
	for mock := range f.refutedOf {
		if !f.unserved[mock] {
			return false
		}
		found := false
		for _, x := range raw {
			if x.mock != mock {
				continue
			}
			sr, ok := x.roles[r.Path]
			if !ok || sr.Value != r.Value || sr.Kind != "" && sr.Kind != kind {
				return false
			}
			found = true
		}
		if !found {
			return false
		}
	}
	return true
}
