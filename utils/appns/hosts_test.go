package appns

import (
	"strings"
	"testing"
)

// The hosts lines distributions ship, and the shapes glibc reads besides:
// each is either kept as it is (no copy) or replaced by the line keploy can
// see, with every other line of the file byte for byte as it was.
func TestHostsView(t *testing.T) {
	for _, tc := range []struct {
		name string
		conf string
		want string // "" when the host's file is to be left alone
	}{
		{
			name: "fedora",
			conf: "passwd:     files systemd\nhosts:      files myhostname resolve [!UNAVAIL=return] dns\nnetworks:   files dns\n",
			want: "passwd:     files systemd\nhosts:      files myhostname dns\nnetworks:   files dns\n",
		},
		{
			name: "fedora with avahi: mdns4_minimal (avahi-daemon) is left out, with its action",
			conf: "hosts:      files myhostname mdns4_minimal [NOTFOUND=return] resolve [!UNAVAIL=return] dns\n",
			want: "hosts:      files myhostname dns\n",
		},
		{
			name: "arch: mymachines (systemd-machined) is left out, files keeps its place",
			conf: "hosts: mymachines resolve [!UNAVAIL=return] files myhostname dns\n",
			want: "hosts: files myhostname dns\n",
		},
		{
			name: "ubuntu with libnss-resolve",
			conf: "hosts:          files mdns4_minimal [NOTFOUND=return] resolve [!UNAVAIL=return] dns myhostname\n",
			want: "hosts:          files dns myhostname\n",
		},
		{
			name: "ubuntu desktop",
			conf: "hosts:          files mdns4_minimal [NOTFOUND=return] dns\nnetworks:       files\n",
		},
		{
			name: "ubuntu server",
			conf: "hosts:          files dns\n",
		},
		{
			name: "debian desktop",
			conf: "# /etc/nsswitch.conf\npasswd:         files\nhosts:          files mdns4_minimal [NOTFOUND=return] dns myhostname\n",
		},
		{
			name: "alpine (musl reads none; glibc compat ships this)",
			conf: "hosts: files dns\n",
		},
		{name: "dns only", conf: "hosts: dns\n"},
		{name: "files and dns only, the other way round", conf: "hosts: dns files\n"},
		{name: "resolve after dns: dns, which keploy sees, has the DNS", conf: "hosts: files dns resolve\n"},
		{name: "resolve after dns, no files", conf: "hosts: dns resolve [!UNAVAIL=return] myhostname\n"},
		{name: "no hosts line", conf: "passwd: files\ngroup: files\n"},
		{name: "empty file", conf: ""},
		{name: "only a comment names resolve", conf: "#hosts: files resolve [!UNAVAIL=return] dns\nhosts: files dns\n"},
		{name: "another database names resolve", conf: "networks: files resolve\nhosts: files dns\n"},
		{name: "a longer database name", conf: "hostsfoo: resolve\n"},
		{name: "a module named like it", conf: "hosts: files resolved dns\n"},
		{name: "an action never closed: glibc rejects the line", conf: "hosts: files [NOTFOUND=return resolve dns\n"},
		{name: "an action never closed after resolve: glibc rejects the line", conf: "hosts: files resolve [!UNAVAIL=return\n"},
		{name: "an action first: glibc reads no module", conf: "hosts: [NOTFOUND=return] files resolve dns\n"},
		{name: "two actions in a row: glibc reads no further", conf: "hosts: files [NOTFOUND=continue] [SUCCESS=return] resolve dns\n"},
		{name: "an action misspelt: glibc rejects the line", conf: "hosts: files resolve [!UNAVAIL=retrun] dns\n"},
		{name: "an empty action: glibc rejects the line", conf: "hosts: files resolve [] dns\n"},
		{name: "an action with no '=': glibc rejects the line", conf: "hosts: files resolve [NOTFOUND] dns\n"},
		{name: "an unknown status: glibc rejects the line", conf: "hosts: files resolve [FOUND=return] dns\n"},
		{name: "a blank after '!': glibc rejects the line", conf: "hosts: files resolve [! UNAVAIL=return] dns\n"},
		{
			name: "no blank after the colon, tabs, actions against the names, a comment, CRLF",
			conf: "  hosts:files\tresolve[!UNAVAIL=return]dns # resolved first\r\npasswd: files\r\n",
			want: "  hosts:files myhostname dns # resolved first\r\npasswd: files\r\n",
		},
		{
			name: "a blank before the colon",
			conf: "hosts : resolve dns\n",
			want: "hosts : files myhostname dns\n",
		},
		{
			name: "no colon (glibc skips blanks and colons alike)",
			conf: "hosts resolve dns\n",
			want: "hosts files myhostname dns\n",
		},
		{
			name: "blanks inside an action",
			conf: "hosts: files resolve [ !UNAVAIL = return ] dns\n",
			want: "hosts: files myhostname dns\n",
		},
		{
			name: "no dns",
			conf: "hosts: files resolve\n",
			want: "hosts: files myhostname dns\n",
		},
		{
			name: "no files and no myhostname: resolved answered for both",
			conf: "hosts: resolve [!UNAVAIL=return] dns\n",
			want: "hosts: files myhostname dns\n",
		},
		{
			name: "libvirt kept, in the line's order",
			conf: "hosts: files libvirt resolve [!UNAVAIL=return] myhostname libvirt_guest dns\n",
			want: "hosts: files libvirt myhostname libvirt_guest dns\n",
		},
		{
			name: "directory, NetBIOS and mDNS modules are left out",
			conf: "hosts: files sss wins resolve [!UNAVAIL=return] ldap nis nisplus mdns mdns4 mdns6 mdns_minimal mdns6_minimal dns\n",
			want: "hosts: files myhostname dns\n",
		},
		{
			name: "a module not named in the rule is left out, with its action",
			conf: "hosts: files docker [NOTFOUND=continue] resolve [!UNAVAIL=return] dns\n",
			want: "hosts: files myhostname dns\n",
		},
		{
			name: "IPC modules with no resolve before dns: the line is kept as it is",
			conf: "hosts: mymachines sss files mdns4_minimal [NOTFOUND=return] dns resolve\n",
		},
		{
			name: "resolve twice, and another module twice as the host has it",
			conf: "hosts: files myhostname resolve files resolve [!UNAVAIL=return] dns\n",
			want: "hosts: files myhostname files dns\n",
		},
		{
			name: "every hosts line that names resolve (glibc versions differ on which of several they read)",
			conf: "hosts: files dns\nhosts: files myhostname resolve [!UNAVAIL=return] dns\nhosts: resolve\n",
			want: "hosts: files dns\nhosts: files myhostname dns\nhosts: files myhostname dns\n",
		},
		{
			name: "no line ending at the end",
			conf: "hosts: files resolve dns",
			want: "hosts: files myhostname dns",
		},
		{
			name: "actions of several items, in any case, with blanks",
			conf: "hosts: files resolve [ !unavail = Return  NOTFOUND=continue ] dns\n",
			want: "hosts: files myhostname dns\n",
		},
		{
			name: "merge, and a status negated",
			conf: "hosts: files [SUCCESS=merge] resolve [!UNAVAIL=return] dns\n",
			want: "hosts: files [SUCCESS=merge] myhostname dns\n",
		},
		{
			name: "resolve before dns and after it",
			conf: "hosts: files resolve dns resolve\n",
			want: "hosts: files myhostname dns\n",
		},
		{
			name: "an action of a module kept keeps its blanks",
			conf: "hosts: files [ SUCCESS = return ] resolve dns [ NOTFOUND=return ]\n",
			want: "hosts: files [ SUCCESS = return ] myhostname dns [ NOTFOUND=return ]\n",
		},
		{
			name: "leading blanks and blanks before the line end",
			conf: "\thosts:\tresolve dns  \n",
			want: "\thosts:\tfiles myhostname dns  \n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, differs := HostsView([]byte(tc.conf))
			if tc.want == "" {
				if differs {
					t.Fatalf("HostsView replaced a line of a file keploy can already see:\n%q\nbecame\n%q", tc.conf, v.Conf)
				}
				return
			}
			if !differs {
				t.Fatalf("HostsView left alone a file whose hosts line keploy cannot see:\n%q", tc.conf)
			}
			if string(v.Conf) != tc.want {
				t.Fatalf("HostsView(%q)\n got %q\nwant %q", tc.conf, v.Conf, tc.want)
			}
		})
	}
}

// What the log lines say of a copy: the first line replaced, as glibc reads
// it, before and after.
func TestHostsViewNamesTheLineItReplaced(t *testing.T) {
	v, differs := HostsView([]byte("passwd: files\nhosts:      files myhostname resolve [!UNAVAIL=return]   dns # x\nhosts: resolve\n"))
	if !differs {
		t.Fatal("HostsView left a Fedora hosts line alone")
	}
	if want := "hosts: files myhostname resolve [!UNAVAIL=return] dns"; v.From != want {
		t.Errorf("From = %q, want %q", v.From, want)
	}
	if want := "hosts: files myhostname dns"; v.To != want {
		t.Errorf("To = %q, want %q", v.To, want)
	}
	if strings.Contains(string(v.Conf), "resolve") {
		t.Errorf("the copy still names resolve:\n%s", v.Conf)
	}
}

// A file authselect generated whose hosts line is the one earlier keploy
// versions wrote (and a killed run left) gives that away, as no authselect
// profile writes it; the same line anywhere else is anyone's, and so is a
// line like it.
func TestLeftByEarlierKeploy(t *testing.T) {
	const header = "# Generated by authselect\n# Do not modify this file manually, use authselect instead. Any user changes will be overwritten.\n"
	for _, tc := range []struct {
		name string
		conf string
		want bool
	}{
		{name: "authselect's file, rewritten", conf: header + "passwd:     files systemd\nhosts: files dns\nnetworks:   files\n", want: true},
		{name: "authselect's file, rewritten, its last line with no line end", conf: header + "passwd:     files systemd\nhosts: files dns", want: true},
		{name: "authselect's own line", conf: header + "hosts:      files myhostname resolve [!UNAVAIL=return] dns\n"},
		{name: "authselect's file, a line like it, aligned", conf: header + "hosts:      files dns\n"},
		{name: "authselect's file, a line like it, a blank after it", conf: header + "hosts: files dns \n"},
		{name: "authselect's file, a line like it, CRLF", conf: header + "hosts: files dns\r\n"},
		{name: "authselect's file, only a comment has it", conf: header + "#hosts: files dns\nhosts:      files myhostname resolve [!UNAVAIL=return] dns\n"},
		{name: "the line in a file authselect did not generate (Alpine's glibc compat, a CI lane's)", conf: "hosts: files dns\n"},
		{name: "authselect's header, not first", conf: "# edited\n" + header + "hosts: files dns\n"},
		{name: "empty file", conf: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := leftByEarlierKeploy([]byte(tc.conf)); got != tc.want {
				t.Errorf("leftByEarlierKeploy(%q) = %v, want %v", tc.conf, got, tc.want)
			}
		})
	}
}
