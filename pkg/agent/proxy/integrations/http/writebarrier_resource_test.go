package http

import "testing"

func TestResourceOfPairsMutationsWithReads(t *testing.T) {
	cases := []struct {
		a, b string
		same bool
	}{
		{"gqlRootField=createATGApp", "operationName=MyApps", true},
		{"operationName=CreateATGApp", "gqlRootField=apps", true},
		{"gqlRootField=createATGApp", "gqlRootField=appById", true},
		{"gqlRootField=createRole", "operationName=MyApps", false},
		{"gqlRootField=deleteRoleBinding", "gqlRootField=apps", false},
		{"gqlRootField=createUser", "gqlRootField=apps", false},
		{"gqlRootField=createRole", "gqlRootField=roles", true},
	}
	for _, c := range cases {
		ra, rb := resourceOf(c.a), resourceOf(c.b)
		if (ra == rb) != c.same {
			t.Errorf("resourceOf(%q)=%q vs resourceOf(%q)=%q: want same=%v", c.a, ra, c.b, rb, c.same)
		}
	}
}
