package learn

// A field keeps one learned role across its sibling places. A sibling is a
// recording of the same call as the one being classified: the same
// statement, on the same connection or in the same test, which its
// protocol's learn step tells (Raw.Sibling). Where the sibling's learn step
// learned a place as noise, as an echo, or as a plain read of the
// application's clock, and the call holds an equal value at the same place,
// the value is learned the same way, never as a value carried from the
// sibling.
//
// An application that reads its clock once per statement in a coarser unit
// than its statements run apart in (Sequelize's JavaScript Date, to the
// millisecond: three role INSERTs made in one millisecond, sent one after
// another on one connection) writes one stamp at the same place of each.
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

// SiblingRole is what a sibling's learn step learned one of its places as
// (Raw.Sibling): its kind, and the value the sibling recorded there.
type SiblingRole struct {
	Kind  Kind
	Value string
}

// Sibling is r, a recording of the same call as the one being classified
// (its protocol's learn step tells: the same statement, on the same
// connection or in the same test), stating the roles its learn step learned
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
