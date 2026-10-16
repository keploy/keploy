package realdeps

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseReadsEachKindOfEntry(t *testing.T) {
	l, err := Parse([]string{
		"localhost:5432",
		"10.0.4.7:6379",
		"[::1]:9000",
		"[::ffff:10.0.0.9]:80",
		"Auth.Example.Com.:8443",
		"db:5432",
		"container:docker.io/library/Redis",
		"container:localhost:5000/team/app",
		"auth.example.com:8443", // the same entry again
	})
	require.NoError(t, err)
	require.Equal(t, []string{
		"localhost:5432", "10.0.4.7:6379", "[::1]:9000", "10.0.0.9:80",
		"auth.example.com:8443", "db:5432", "container:redis", "container:localhost:5000/team/app",
	}, l.Labels())
	require.Equal(t, []string{"auth.example.com", "db"}, l.Names())

	// localhost is 127.0.0.1 and ::1, whatever the hosts file says.
	for _, a := range []string{"127.0.0.1:5432", "[::1]:5432", "[::ffff:127.0.0.1]:5432"} {
		label, ok := l.address(netip.MustParseAddrPort(a))
		require.True(t, ok, a)
		require.Equal(t, "localhost:5432", label)
	}
	_, ok := l.address(netip.MustParseAddrPort("127.0.0.2:5432"))
	require.False(t, ok, "only 127.0.0.1 and ::1 are localhost")
	_, ok = l.address(netip.MustParseAddrPort("127.0.0.1:5433"))
	require.False(t, ok, "an address entry names its port")

	label, ok := l.named("AUTH.example.com.", 8443)
	require.True(t, ok)
	require.Equal(t, "auth.example.com:8443", label)
	_, ok = l.named("auth.example.com", 443)
	require.False(t, ok)

	label, ok = l.container("redis")
	require.True(t, ok)
	require.Equal(t, "container:redis", label)

	require.True(t, l.IsName("auth.example.com."))
	require.True(t, l.IsName("DB"))
	require.False(t, l.IsName("other.example.com"))
	require.True(t, l.HasContainers())
}

func TestParseRefusesWhatIsNotAnEntry(t *testing.T) {
	for _, s := range []string{
		"",
		"localhost",              // no port
		"auth.example.com",       // no port
		"auth.example.com:0",     // port 0
		"auth.example.com:65536", // past the last port
		"auth.example.com:08443", // not the port's own spelling
		"*.example.com:443",      // a wildcard
		"bad_-name.:443x",        // no port
		"-lead.example.com:443",  // a label starting with '-'
		"container:",             // no repository
		"container:redis:7",      // a tag
		"container:redis@sha256", // a digest
		"[fe80::1%eth0]:80",      // a zone
	} {
		_, err := Parse([]string{s})
		require.ErrorIs(t, err, ErrBadEntry, s)
	}
}

func TestAnEmptyListListsNothing(t *testing.T) {
	l, err := Parse(nil)
	require.NoError(t, err)
	require.True(t, l.Empty())
	var nilList *List
	require.True(t, nilList.Empty())
	require.False(t, nilList.IsName("x"))
	require.Nil(t, nilList.Labels())
}

func TestCanonicalName(t *testing.T) {
	require.Equal(t, "auth.example.com", CanonicalName("Auth.Example.Com."))
	require.Equal(t, "", CanonicalName("10.0.0.1"), "an IP literal names nothing")
	require.Equal(t, "", CanonicalName(""))
	require.Equal(t, "", CanonicalName("a b"))
}
