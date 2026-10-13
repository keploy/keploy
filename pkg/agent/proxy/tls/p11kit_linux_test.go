//go:build linux

package tls

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/unix"
)

// These tests run swapP11KitTrust against p11-kit trust stores built in a temp
// directory, each with an update-ca-trust written the way a distro writes its
// own — Fedora 44 ("trust extract", directory-hash, links in
// /etc/pki/tls/certs), RHEL 9 ("trust extract", directory-hash and its two
// compatibility links), Amazon Linux 2023 ("p11-kit extract", unquoted, the
// same) and RHEL 8 ("p11-kit extract", bundles only) — which keploy reads and
// must never run. p11-kit itself is played in-process (p11Store.extract,
// through p11kitRunExtract): it extracts from the store's anchor directories
// what the real one does, in the shapes the swap handles — bundle files, and a
// directory of <label>.pem files with <hash>.<n> links, the same label taking
// .1, .2 — with hashes a test can choose, to make two CAs share one. (The real
// p11-kit is what the e2e runs.)
//
// What they prove: after every single change the swap makes to the live store,
// every CA that is trusted both before and after the swap is in every bundle
// and is found the way OpenSSL looks a CA up in a hashed directory (try
// <hash>.0, <hash>.1, ..., stop at the first that is missing), by each of its
// hashes, in directory-hash and in the certs directory; every name that is in
// the store before and after is only ever replaced by a rename, never removed
// or rewritten first; and at the end the store is exactly what the distro's
// own tool makes of the same anchors.

// ---------------------------------------------------------------------------
// the trust tool
// ---------------------------------------------------------------------------

// fakeCA is one CA of the store as the trust tool sees it.
type fakeCA struct {
	cn     string
	der    []byte
	pem    []byte
	hashes []string // the names of its links: p11-kit makes two, one per OpenSSL hash function
}

// extract plays "trust extract" / "p11-kit extract" for the swap, as the
// wrapper's tool would run it: it fails as p11-kit does on a destination that
// exists, and fails the run if the swap ever runs another tool than the one
// the wrapper names, extracts over a live output, or leaves out the wrapper's
// P11_KIT_NO_USER_CONFIG=1 (which keeps a user's own p11-kit configuration out
// of the system store).
func (st *p11Store) extract(cmd *exec.Cmd) ([]byte, error) {
	args := cmd.Args[1:]
	if len(args) < 2 || args[0] != "extract" {
		return []byte(fmt.Sprintf("fake trust: unexpected arguments %q", args)), errors.New("exit status 2")
	}
	if cmd.Path != st.tool() {
		return []byte("fake trust: run as " + cmd.Path + ", but the wrapper runs " + st.tool()), errors.New("exit status 2")
	}
	if !slices.Contains(cmd.Env, "P11_KIT_NO_USER_CONFIG=1") {
		return []byte("fake trust: run without P11_KIT_NO_USER_CONFIG=1, as the wrapper never is"), errors.New("exit status 2")
	}
	dest := args[len(args)-1]
	format := ""
	for _, a := range args {
		if strings.HasPrefix(a, "--format=") {
			format = strings.TrimPrefix(a, "--format=")
		}
		if a == "--overwrite" {
			return []byte("fake trust: keploy must never extract over a live output"), errors.New("exit status 2")
		}
	}
	if format == st.failFormat {
		return []byte("p11-kit: couldn't extract " + format), errors.New("exit status 1")
	}
	if _, err := os.Lstat(dest); err == nil {
		return []byte("p11-kit: couldn't complete writing of file: " + dest + ": File exists"), errors.New("exit status 1")
	}
	cas, err := readFakeCAs([]string{st.anchors, st.roots}, st.hashes)
	if err == nil {
		err = writeFakeOutput(format, dest, cas)
	}
	if err != nil {
		return []byte(err.Error()), errors.New("exit status 1")
	}
	return nil, nil
}

// readFakeCAs reads every certificate in the anchor directories, in order and
// by file name within each.
func readFakeCAs(dirs []string, hashesFile string) ([]fakeCA, error) {
	overrides := map[string][]string{}
	if hashesFile != "" {
		data, err := os.ReadFile(hashesFile)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			if f := strings.Split(line, "|"); len(f) == 2 {
				overrides[f[0]] = strings.Fields(f[1])
			}
		}
	}
	var cas []fakeCA
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			block, _ := pem.Decode(data)
			if block == nil {
				continue
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				continue
			}
			cas = append(cas, fakeCA{cn: cert.Subject.CommonName, der: block.Bytes, pem: pem.EncodeToMemory(block),
				hashes: fakeHashes(cert, overrides)})
		}
	}
	return cas, nil
}

// fakeHashes are a CA's two link names: derived from its subject, like
// OpenSSL's, unless the test chose them.
func fakeHashes(cert *x509.Certificate, overrides map[string][]string) []string {
	if h, ok := overrides[cert.Subject.CommonName]; ok {
		return h
	}
	a := sha256.Sum256(cert.RawSubject)
	b := sha256.Sum256(append([]byte("old"), cert.RawSubject...))
	return []string{hex.EncodeToString(a[:4]), hex.EncodeToString(b[:4])}
}

func fakeLabel(cn string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.' {
			return r
		}
		return '_'
	}, cn)
}

func writeFakeOutput(format, dest string, cas []fakeCA) error {
	var buf bytes.Buffer
	switch format {
	case "pem-bundle", "openssl-bundle":
		for _, ca := range cas {
			fmt.Fprintf(&buf, "# %s\n%s", ca.cn, ca.pem)
		}
	case "java-cacerts":
		// Shaped like a keystore's header, which is all anything here reads.
		_ = binary.Write(&buf, binary.BigEndian, []uint32{0xFEEDFEED, 2, uint32(len(cas))})
		for _, ca := range cas {
			buf.Write(ca.der)
		}
	case "edk2-cacerts":
		for _, ca := range cas {
			buf.Write(ca.der)
		}
	case "pem-directory-hash":
		if err := os.Mkdir(dest, 0o755); err != nil {
			return err
		}
		used := map[string]bool{}
		next := map[string]int{}
		for _, ca := range cas {
			name := fakeLabel(ca.cn) + ".pem"
			for i := 1; used[name]; i++ {
				name = fakeLabel(ca.cn) + "." + strconv.Itoa(i) + ".pem"
			}
			used[name] = true
			if err := os.WriteFile(filepath.Join(dest, name), ca.pem, 0o444); err != nil {
				return err
			}
			for _, h := range ca.hashes {
				if err := os.Symlink(name, filepath.Join(dest, h+"."+strconv.Itoa(next[h]))); err != nil {
					return err
				}
				next[h]++
			}
		}
		// p11-kit leaves the directory read-only.
		return os.Chmod(dest, 0o555)
	default:
		return fmt.Errorf("fake trust: unknown format %q", format)
	}
	return os.WriteFile(dest, buf.Bytes(), 0o444)
}

// ---------------------------------------------------------------------------
// a p11-kit store in a temp directory
// ---------------------------------------------------------------------------

type p11Layout int

const (
	fedora44 p11Layout = iota // -o; trust extract, directory-hash, links in the certs directory
	rhel10                    // as Fedora 44, its fill loop written "while read link"
	rhel9                     // -o; trust extract, directory-hash with two compatibility links
	al2023                    // no -o; p11-kit extract, unquoted; as RHEL 9
	rhel8                     // no -o; p11-kit extract, unquoted; bundles only
)

var allLayouts = []p11Layout{fedora44, rhel10, rhel9, al2023, rhel8}

func (l p11Layout) String() string {
	return [...]string{"fedora44", "rhel10", "rhel9", "al2023", "rhel8"}[l]
}

// distroOutput is one extraction every distro's update-ca-trust runs, with its
// arguments as the wrapper writes them.
type distroOutput struct {
	rel  string
	args string
	dir  bool
}

// distroOutputs are in the order the wrappers run them. They are the test's
// own copy, not the package's: the store, its wrapper and the reference the
// swap is compared with are all built from this.
var distroOutputs = []distroOutput{
	{rel: "openssl/ca-bundle.trust.crt", args: "--format=openssl-bundle --filter=certificates --overwrite --comment"},
	{rel: "pem/tls-ca-bundle.pem", args: "--format=pem-bundle --filter=ca-anchors --overwrite --comment --purpose server-auth"},
	{rel: "pem/email-ca-bundle.pem", args: "--format=pem-bundle --filter=ca-anchors --overwrite --comment --purpose email"},
	{rel: "pem/objsign-ca-bundle.pem", args: "--format=pem-bundle --filter=ca-anchors --overwrite --comment --purpose code-signing"},
	{rel: "java/cacerts", args: "--format=java-cacerts --filter=ca-anchors --overwrite --purpose server-auth"},
	{rel: "edk2/cacerts.bin", args: "--format=edk2-cacerts --filter=ca-anchors --overwrite --purpose=server-auth"},
	{rel: "pem/directory-hash", dir: true, args: "--format=pem-directory-hash --filter=ca-anchors --overwrite --purpose server-auth"},
}

func (l p11Layout) outputs() []distroOutput {
	if l == rhel8 {
		return distroOutputs[:6]
	}
	return distroOutputs
}

func (l p11Layout) p11kitExtract() bool { return l == rhel8 || l == al2023 }
func (l p11Layout) outputOption() bool  { return !l.p11kitExtract() }
func (l p11Layout) compatLinks() bool   { return l == rhel9 || l == al2023 }
func (l p11Layout) certsLinked() bool   { return l == fedora44 || l == rhel10 }

type p11Store struct {
	layout        p11Layout
	root          string
	anchors       string // /etc/pki/ca-trust/source/anchors: where agents write
	roots         string // the distro's own CAs
	hashes        string // the test's choice of link names, "<CN>|<hash> <hash>" per line
	extracted     string // $DEST in the wrapper; a symlink to extractedReal when built behind one
	extractedReal string
	certsDir      string // /etc/pki/tls/certs
	trust         string // stand-ins for /usr/bin/trust and /usr/bin/p11-kit; played in-process, never run
	p11kit        string
	wrapper       string // update-ca-trust: read by keploy, and must never run
	wrapperLog    string
	extraLines    []string          // more lines for the wrapper
	noCertsFill   bool              // the wrapper sets DEST_CERTS but has no loop that fills it
	argsFor       map[string]string // other arguments for an output in the wrapper
	failFormat    string            // the --format= the trust tool fails, if any
	stage         func(dir string)  // changes what the wrapper's -o run leaves in dir, before keploy sees it
}

func (st *p11Store) dirHash() string { return filepath.Join(st.extractedReal, "pem", "directory-hash") }

// tool is the extraction tool the store's wrapper names.
func (st *p11Store) tool() string {
	if st.layout.p11kitExtract() {
		return st.p11kit
	}
	return st.trust
}

type p11Option func(*p11Store)

// behindSymlink puts the store's outputs behind a symlink, as a machine whose
// /etc/pki is one does: the wrapper links /etc/pki/tls/certs to the files
// directory-hash resolves to, not to the path it names them by.
func behindSymlink(st *p11Store) {
	st.extractedReal = filepath.Join(st.root, "real-extracted")
}

// withWrapperLines adds lines to the store's update-ca-trust.
func withWrapperLines(lines ...string) p11Option {
	return func(st *p11Store) { st.extraLines = append(st.extraLines, lines...) }
}

// withoutCertsFill leaves DEST_CERTS in the wrapper but takes out the loop that
// fills it.
func withoutCertsFill(st *p11Store) { st.noCertsFill = true }

// withOutputArgs gives an output other arguments in the wrapper.
func withOutputArgs(rel, args string) p11Option {
	return func(st *p11Store) {
		if st.argsFor == nil {
			st.argsFor = map[string]string{}
		}
		st.argsFor[rel] = args
	}
}

// newP11Store builds a store in the given layout holding nRoots distro CAs, as
// the distro's tool would have left it, and points p11-kit at the test's
// in-process stand-in.
func newP11Store(t *testing.T, layout p11Layout, nRoots int, opts ...p11Option) *p11Store {
	t.Helper()
	root := t.TempDir()
	st := &p11Store{
		layout:     layout,
		root:       root,
		anchors:    filepath.Join(root, "source", "anchors"),
		roots:      filepath.Join(root, "system-roots"),
		hashes:     filepath.Join(root, "hashes"),
		extracted:  filepath.Join(root, "extracted"),
		certsDir:   filepath.Join(root, "tls", "certs"),
		trust:      filepath.Join(root, "bin", "trust"),
		p11kit:     filepath.Join(root, "bin", "p11-kit"),
		wrapper:    filepath.Join(root, "bin", "update-ca-trust"),
		wrapperLog: filepath.Join(root, "wrapper.log"),
	}
	st.extractedReal = st.extracted
	for _, o := range opts {
		o(st)
	}
	for _, d := range []string{st.anchors, st.roots, st.certsDir, filepath.Dir(st.trust), st.extractedReal} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if st.extractedReal != st.extracted {
		if err := os.Symlink(st.extractedReal, st.extracted); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < nRoots; i++ {
		st.addCA(t, st.roots, fmt.Sprintf("root-%c.crt", 'a'+i), fmt.Sprintf("Distro Root %c", 'A'+i))
	}
	stub := "#!/bin/sh\necho 'played in-process; nothing should run this' >&2\nexit 2\n"
	for p, body := range map[string]string{st.trust: stub, st.p11kit: stub, st.wrapper: st.wrapperScript()} {
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	st.distroExtract(t, st.extractedReal, st.certsDir)

	prevRun := p11kitRunExtract
	p11kitRunExtract = st.run
	t.Cleanup(func() {
		p11kitRunExtract = prevRun
		p11kitSwapStep = nil
		_ = removeTree(root)
	})
	return st
}

// wrapperScript is the store's update-ca-trust, written as the layout's distro
// writes its own — the -o option, the blocks that run only on the live store,
// Fedora's compatibility block, a usage text — except that it fails at once if
// anything runs it.
func (st *p11Store) wrapperScript() string {
	var b strings.Builder
	b.WriteString("#!/usr/bin/sh\nset -eu\n# For backwards compatibility reasons, future versions of this script must\n# support the syntax \"update-ca-trust extract\"\n")
	fmt.Fprintf(&b, "DEST=%s\n", st.extracted)
	if st.layout.certsLinked() {
		fmt.Fprintf(&b, "DEST_CERTS=%s\n", st.certsDir)
	}
	b.WriteString("export P11_KIT_NO_USER_CONFIG=1\n")
	fmt.Fprintf(&b, "echo ran >> %q\nexit 1\n", st.wrapperLog)
	if st.layout.outputOption() {
		b.WriteString("usage() {\n\tfold -s -w 76 >&2 <<-EOF\n\t\tUpdate the system trust store in $DEST.\n\t\textract: produce updated versions of the files below $DEST.\n\tEOF\n}\n")
	}
	b.WriteString("extract() {\n")
	if st.layout.outputOption() {
		b.WriteString("\tUSER_DEST=\n\tcompat=\n\twhile [ $# -ne 0 ]; do\n\t    case \"$1\" in\n\t\t\"-o\"|\"--output\")\n" +
			"\t\t  USER_DEST=$2\n\t\t  shift 2\n\t\t  continue\n\t\t  ;;\n\t\t\"--rhbz2387674\")\n\t\t  compat=\"true\"\n\t\t  shift\n\t\t  continue\n\t\t  ;;\n\t    esac\n\tdone\n")
		b.WriteString("\tif [ -n \"$USER_DEST\" ]; then\n\t\tDEST=$USER_DEST\n\t\t/usr/bin/mkdir -p \"$DEST\"/openssl \"$DEST\"/pem \"$DEST\"/java \"$DEST\"/edk2\n\tfi\n")
	}
	if st.layout.certsLinked() {
		b.WriteString("\tif [ -z \"$USER_DEST\" ]; then\n\t\tfind \"$DEST_CERTS\" -type l -regextype posix-extended \\\n" +
			"\t\t     -regex '.*/[0-9a-f]{8}\\.[0-9]+' -exec rm -f {} \\;\n\tfi\n")
	}
	for _, o := range st.layout.outputs() {
		args := o.args
		if a, ok := st.argsFor[o.rel]; ok {
			args = a
		}
		if st.layout.p11kitExtract() {
			fmt.Fprintf(&b, "%s extract %s $DEST/%s\n", st.p11kit, args, o.rel)
		} else {
			fmt.Fprintf(&b, "\t%s extract %s \"$DEST/%s\"\n", st.trust, args, o.rel)
		}
	}
	if st.layout.compatLinks() {
		if st.layout.outputOption() {
			b.WriteString("\tif [ -n \"$USER_DEST\" ]; then\n\t    /usr/bin/chmod u+w \"$DEST/pem/directory-hash\"\n\tfi\n")
		}
		for _, name := range []string{"ca-certificates.crt", "ca-bundle.crt"} {
			if st.layout == al2023 {
				fmt.Fprintf(&b, "/usr/bin/ln -s ../tls-ca-bundle.pem $DEST/pem/directory-hash/%s\n", name)
			} else {
				fmt.Fprintf(&b, "\t/usr/bin/ln -s ../tls-ca-bundle.pem \"$DEST/pem/directory-hash/%s\"\n", name)
			}
		}
		if st.layout.outputOption() {
			b.WriteString("\tif [ -n \"$USER_DEST\" ]; then\n\t    /usr/bin/chmod u-w \"$DEST/pem/directory-hash\"\n\tfi\n")
		}
	}
	if st.layout.certsLinked() {
		b.WriteString("\tif [ -n \"$compat\" ]; then\n\t\trhbz2387674_msg\n\t\tln -sf \"$DEST/pem/tls-ca-bundle.pem\" \"$DEST_CERTS/../cert.pem\"\n" +
			"\t\tln -sf \"$DEST/openssl/ca-bundle.trust.crt\" \"$DEST_CERTS/ca-bundle.trust.crt\"\n\tfi\n")
		if !st.noCertsFill {
			read := "while IFS= read -r link; do"
			if st.layout == rhel10 {
				read = "while read link; do"
			}
			b.WriteString("\tif [ -z \"$USER_DEST\" ]; then\n\t\tfind \"$DEST/pem/directory-hash\" -type l -regextype posix-extended \\\n" +
				"\t\t     -regex '.*/[0-9a-f]{8}\\.[0-9]+' | " + read + "\n\t\t\ttarget=$(readlink -f \"$link\")\n" +
				"\t\t\tnew_link=\"$DEST_CERTS/$(basename \"$link\")\"\n\t\t\tln -s \"$target\" \"$new_link\"\n\t\tdone\n\tfi\n")
		}
	}
	for _, l := range st.extraLines {
		b.WriteString("\t" + l + "\n")
	}
	b.WriteString("}\n")
	return b.String()
}

// run plays, in-process, whatever the swap runs to fill its staging
// directory: the wrapper itself with -o, where the layout has it, or one of
// the wrapper's extractions.
func (st *p11Store) run(cmd *exec.Cmd) ([]byte, error) {
	if cmd.Path != st.wrapper {
		return st.extract(cmd)
	}
	args := cmd.Args[1:]
	if !st.layout.outputOption() || len(args) != 3 || args[0] != "extract" || args[1] != "-o" {
		return []byte(fmt.Sprintf("fake update-ca-trust: unexpected arguments %q", args)), errors.New("exit status 2")
	}
	if err := st.wrapperOutput(args[2]); err != nil {
		return []byte(err.Error()), errors.New("exit status 1")
	}
	if st.stage != nil {
		st.stage(args[2])
	}
	return nil, nil
}

// wrapperOutput writes into dir what the layout's wrapper writes under its
// DEST: every output, and the compatibility links — not the links in the certs
// directory, which only its live run makes.
func (st *p11Store) wrapperOutput(dir string) error {
	for _, o := range st.layout.outputs() {
		dest := filepath.Join(dir, o.rel)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		args := []string{"extract"}
		for _, a := range strings.Fields(o.args) {
			if a != "--overwrite" {
				args = append(args, a)
			}
		}
		cmd := exec.Command(st.tool(), append(args, dest)...)
		cmd.Env = append(os.Environ(), "P11_KIT_NO_USER_CONFIG=1")
		if out, err := st.extract(cmd); err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
	}
	if st.layout.compatLinks() {
		dh := filepath.Join(dir, "pem", "directory-hash")
		if err := os.Chmod(dh, 0o755); err != nil {
			return err
		}
		for _, name := range []string{"ca-certificates.crt", "ca-bundle.crt"} {
			if err := os.Symlink("../tls-ca-bundle.pem", filepath.Join(dh, name)); err != nil {
				return err
			}
		}
		return os.Chmod(dh, 0o555)
	}
	return nil
}

// addCA writes a new CA with this CN into dir under name and returns it.
func (st *p11Store) addCA(t *testing.T, dir, name, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	return der
}

// setHashes makes the trust tool give the CA with this CN these link names.
func (st *p11Store) setHashes(t *testing.T, cn string, hashes ...string) {
	t.Helper()
	f, err := os.OpenFile(st.hashes, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	fmt.Fprintf(f, "%s|%s\n", cn, strings.Join(hashes, " "))
}

// distroExtract writes, under extracted and certsDir, what a live run of the
// distro's tool makes of the store's anchors: every output of the layout, the
// compatibility links, and the certs directory's links.
func (st *p11Store) distroExtract(t *testing.T, extracted, certsDir string) {
	t.Helper()
	if err := st.wrapperOutput(extracted); err != nil {
		t.Fatalf("the fake trust tool failed: %v", err)
	}
	if st.layout == rhel8 {
		return
	}
	dh := filepath.Join(extracted, "pem", "directory-hash")
	// What the test itself must be able to write to; root writes to the
	// distro's read-only directory-hash as freely.
	if err := os.Chmod(dh, 0o755); err != nil {
		t.Fatal(err)
	}
	if st.layout.certsLinked() {
		entries, err := os.ReadDir(dh)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if !hashLinkName.MatchString(e.Name()) {
				continue
			}
			target, err := filepath.EvalSymlinks(filepath.Join(dh, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(certsDir, e.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// regenerate rebuilds the store from scratch as the distro's tool would.
func (st *p11Store) regenerate(t *testing.T) {
	t.Helper()
	for _, d := range []string{st.extractedReal, st.certsDir} {
		if err := removeTree(d); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	st.distroExtract(t, st.extractedReal, st.certsDir)
}

// cas returns the CAs the store's anchors hold now.
func (st *p11Store) cas(t *testing.T) []fakeCA {
	t.Helper()
	cas, err := readFakeCAs([]string{st.anchors, st.roots}, st.hashes)
	if err != nil {
		t.Fatal(err)
	}
	return cas
}

// layoutOf describes every entry of the store's outputs, with paths relative
// to the store and the store's own root removed from link targets, so that two
// stores can be compared.
func layoutOf(t *testing.T, root string, dirs ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			switch {
			case d.Type()&fs.ModeSymlink != 0:
				target, err := os.Readlink(p)
				if err != nil {
					return err
				}
				out[rel] = "-> " + strings.TrimPrefix(target, root)
			case d.IsDir():
				out[rel] = "dir"
			default:
				data, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				sum := sha256.Sum256(data)
				out[rel] = hex.EncodeToString(sum[:])
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func (st *p11Store) layout0(t *testing.T) map[string]string {
	t.Helper()
	return layoutOf(t, st.root, st.extractedReal, st.certsDir)
}

// expectDistroLayout fails the test unless the store is exactly what the
// distro's tool makes of its anchors now.
func (st *p11Store) expectDistroLayout(t *testing.T) {
	t.Helper()
	ref := filepath.Join(t.TempDir(), "ref")
	refReal := filepath.Join(ref, strings.TrimPrefix(st.extractedReal, st.root))
	refCerts := filepath.Join(ref, strings.TrimPrefix(st.certsDir, st.root))
	for _, d := range []string{refReal, refCerts} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	st.distroExtract(t, refReal, refCerts)
	want := layoutOf(t, ref, refReal, refCerts)
	// The reference's links point into the reference; compare them as the
	// store's own.
	for k, v := range want {
		want[k] = strings.ReplaceAll(v, ref, "")
	}
	got := st.layout0(t)
	var diffs []string
	for k, w := range want {
		if g, ok := got[k]; !ok {
			diffs = append(diffs, "missing "+k+" "+w)
		} else if g != w {
			diffs = append(diffs, "changed "+k+": "+g+", want "+w)
		}
	}
	for k, g := range got {
		if _, ok := want[k]; !ok {
			diffs = append(diffs, "extra "+k+" "+g)
		}
	}
	sort.Strings(diffs)
	if len(diffs) != 0 {
		t.Fatalf("the store is not what the distro's tool makes of the same anchors:\n%s", strings.Join(diffs, "\n"))
	}
}

// openSSLFinds reports whether OpenSSL, given dir as a CApath, finds ca by
// each of its hashes: for each, it tries <hash>.0, <hash>.1, ..., following
// links, and stops at the first name that does not resolve. Every hash must
// lead to the CA — OpenSSL 1.0 and later look up by the first, and whatever
// still uses the second must not lose it either.
func openSSLFinds(dir string, ca fakeCA) bool {
	for _, h := range ca.hashes {
		if !findsVia(dir, h, ca) {
			return false
		}
	}
	return true
}

func findsVia(dir, h string, ca fakeCA) bool {
	for i := 0; ; i++ {
		data, err := os.ReadFile(filepath.Join(dir, h+"."+strconv.Itoa(i)))
		if err != nil {
			return false
		}
		if block, _ := pem.Decode(data); block != nil && bytes.Equal(block.Bytes, ca.der) {
			return true
		}
	}
}

// bundleHolds reports whether the file holds the CA's DER, as PEM or raw.
func bundleHolds(path string, ca fakeCA) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return bytes.Contains(data, ca.pem) || bytes.Contains(data, ca.der)
}

// watchSwap checks, after every change the swap makes to the live store, that
// each CA in keep is still trusted: in every file output, and found by OpenSSL
// in directory-hash and in the certs directory. It returns how many changes it
// saw.
func (st *p11Store) watchSwap(t *testing.T, keep []fakeCA) *int {
	t.Helper()
	changes := 0
	p11kitSwapStep = func(change string) {
		changes++
		for _, ca := range keep {
			for _, o := range st.layout.outputs() {
				if !o.dir && !bundleHolds(filepath.Join(st.extractedReal, o.rel), ca) {
					t.Fatalf("after %q: %s is missing from %s", change, ca.cn, o.rel)
				}
			}
			if st.layout != rhel8 && !openSSLFinds(st.dirHash(), ca) {
				t.Fatalf("after %q: OpenSSL cannot find %s in directory-hash", change, ca.cn)
			}
			if st.layout.certsLinked() && !openSSLFinds(st.certsDir, ca) {
				t.Fatalf("after %q: OpenSSL cannot find %s in its default certificate directory", change, ca.cn)
			}
		}
	}
	return &changes
}

// swap runs the swap the way the install does.
func (st *p11Store) swap(t *testing.T) error {
	t.Helper()
	handled, err := swapP11KitTrust(context.Background(), zap.NewNop(), st.wrapper)
	if !handled {
		t.Fatal("the swap did not take on the p11-kit store")
	}
	return err
}

// intersect returns the CAs in both lists.
func intersect(a, b []fakeCA) []fakeCA {
	var out []fakeCA
	for _, x := range a {
		for _, y := range b {
			if bytes.Equal(x.der, y.der) {
				out = append(out, x)
				break
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// reading the wrapper
// ---------------------------------------------------------------------------

// TestReadP11KitLayout_ReadsEachDistrosWrapper: keploy reads off the distro's
// own update-ca-trust the store it writes, whether it can write its output
// elsewhere (-o), whether its live run fills the certs directory — and, for a
// wrapper without -o, whose work keploy then does itself, every output it
// extracts with the tool it names and every link it adds into the store. None
// of the lines that only look alike (a usage text that names $DEST, the -o
// branch, the --rhbz2387674 block's links into $DEST_CERTS) is taken for one
// it does not know.
func TestReadP11KitLayout_ReadsEachDistrosWrapper(t *testing.T) {
	for _, layout := range allLayouts {
		t.Run(layout.String(), func(t *testing.T) {
			st := newP11Store(t, layout, 1)
			l, err := readP11KitLayout(st.wrapper)
			if err != nil {
				t.Fatal(err)
			}
			if len(l.unknown) != 0 {
				t.Errorf("lines taken for ones keploy does not know: %q", l.unknown)
			}
			if l.dest != st.extracted {
				t.Errorf("DEST read as %q, want %q", l.dest, st.extracted)
			}
			if l.output != layout.outputOption() {
				t.Errorf("read -o as %v, want %v", l.output, layout.outputOption())
			}
			if want := map[bool]string{true: st.certsDir, false: ""}[layout.certsLinked()]; l.certsDir != want {
				t.Errorf("the certs directory read as %q, want %q", l.certsDir, want)
			}
			var generated []string
			var wantRels []string
			for _, o := range layout.outputs() {
				wantRels = append(wantRels, o.rel)
			}
			generated = append(generated, l.generated...)
			if !slices.Equal(generated, wantRels) {
				t.Errorf("outputs it extracts read as %q, want %q", generated, wantRels)
			}
			if layout.outputOption() {
				if len(l.outputs)+len(l.links) != 0 {
					t.Error("with -o the wrapper writes the outputs, but keploy would extract them itself")
				}
				return
			}
			var rels []string
			for _, o := range l.outputs {
				rels = append(rels, o.rel)
				if o.tool != st.tool() {
					t.Errorf("%s would be extracted with %s, but the wrapper uses %s", o.rel, o.tool, st.tool())
				}
			}
			if !slices.Equal(rels, wantRels) {
				t.Errorf("outputs to extract read as %q, want %q", rels, wantRels)
			}
			if got, want := len(l.links), map[bool]int{true: 2, false: 0}[layout.compatLinks()]; got != want {
				t.Errorf("read %d compatibility links, want %d", got, want)
			}
		})
	}
}

// TestSwapP11KitTrust_RunsTheWrapperForWhatItDoesNotKnow: keploy stands in for
// the wrapper only when it knows exactly what the wrapper does, and it knows
// that differently with and without -o. Without -o (Amazon Linux, RHEL 8)
// keploy does the wrapper's work itself, so every line that touches the store
// must be one it can do: another output, other arguments for a known one, the
// same output twice, another link, an extraction or a copy it cannot read, an
// extraction tool that is not there. With -o (Fedora, RHEL 9 and 10) the
// wrapper writes its own output, and what keploy must know is the rest: where
// the store is, and what its live run alone does to the certs directory; and
// what the -o run wrote must be something keploy can put in place exactly as a
// live run would leave it. Anything else: the wrapper is run as it is, keploy
// says what it did not recognise, and the store is not touched before.
func TestSwapP11KitTrust_RunsTheWrapperForWhatItDoesNotKnow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		layout p11Layout
		opts   []p11Option
		setup  func(t *testing.T, st *p11Store)
		says   string
	}{
		{name: "another output", layout: al2023, says: "new-bundle.pem",
			opts: []p11Option{withWrapperLines(`/usr/bin/p11-kit extract --format=pem-bundle --filter=ca-anchors --overwrite --purpose server-auth $DEST/pem/new-bundle.pem`)}},
		{name: "other arguments for a known output", layout: al2023, says: "--purpose email $DEST/pem/tls-ca-bundle.pem",
			opts: []p11Option{withOutputArgs("pem/tls-ca-bundle.pem", "--format=pem-bundle --filter=ca-anchors --overwrite --comment --purpose email")}},
		{name: "the same output twice", layout: al2023, says: "--purpose server-auth $DEST/pem/tls-ca-bundle.pem",
			opts: []p11Option{withWrapperLines(`/usr/bin/p11-kit extract --format=pem-bundle --filter=ca-anchors --overwrite --comment --purpose server-auth $DEST/pem/tls-ca-bundle.pem`)}},
		{name: "another link into the store", layout: al2023, says: "cert.pem",
			opts: []p11Option{withWrapperLines(`/usr/bin/ln -s ../tls-ca-bundle.pem $DEST/pem/cert.pem`)}},
		{name: "a link made with ln -s -f", layout: al2023, says: "-s -f",
			opts: []p11Option{withWrapperLines(`/usr/bin/ln -s -f ../tls-ca-bundle.pem $DEST/pem/directory-hash/ca-bundle.crt`)}},
		{name: "an extraction through a variable", layout: al2023, says: "$TRUST",
			opts: []p11Option{withWrapperLines(`$TRUST extract --format=pem-bundle --filter=ca-anchors --overwrite --comment --purpose email $DEST/pem/email-ca-bundle.pem`)}},
		{name: "a copy into the store", layout: al2023, says: "/usr/bin/cp",
			opts: []p11Option{withWrapperLines(`/usr/bin/cp $DEST/pem/tls-ca-bundle.pem $DEST/pem/extra.pem`)}},
		{name: "no extraction tool", layout: al2023, says: "extractor", setup: func(t *testing.T, st *p11Store) {
			if err := os.Remove(st.tool()); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "DEST set again", layout: fedora44, says: "does not set DEST once",
			opts: []p11Option{withWrapperLines(`DEST=/var/lib/other`)}},
		{name: "DEST exported", layout: fedora44, says: "export DEST",
			opts: []p11Option{withWrapperLines(`export DEST=/var/lib/other`)}},
		{name: "DEST_CERTS without the loop that fills it", layout: fedora44, says: "is not emptied and filled",
			opts: []p11Option{withoutCertsFill}},
		{name: "another use of DEST_CERTS", layout: fedora44, says: "$DEST_CERTS/extra.pem",
			opts: []p11Option{withWrapperLines(`/usr/bin/cp "$DEST/pem/tls-ca-bundle.pem" "$DEST_CERTS/extra.pem"`)}},
		{name: "another line in a live-only block", layout: fedora44, says: "restorecon",
			opts: []p11Option{withWrapperLines(`if [ -z "$USER_DEST" ]; then`, `/usr/sbin/restorecon -R "$DEST"`, `fi`)}},
		{name: "the store's own path", layout: fedora44, says: "literal.pem", setup: func(t *testing.T, st *p11Store) {
			st.extraLines = append(st.extraLines, "/usr/bin/cp /etc/hosts "+st.extracted+"/pem/literal.pem")
			if err := os.WriteFile(st.wrapper, []byte(st.wrapperScript()), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "no DEST", layout: fedora44, says: "does not set DEST once", setup: func(t *testing.T, st *p11Store) {
			script, err := os.ReadFile(st.wrapper)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(st.wrapper, bytes.Replace(script, []byte("DEST="+st.extracted+"\n"), nil, 1), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "-o output without a known output the store has", layout: fedora44, says: "pem/email-ca-bundle.pem", setup: func(t *testing.T, st *p11Store) {
			st.stage = func(dir string) { _ = os.Remove(filepath.Join(dir, "pem", "email-ca-bundle.pem")) }
		}},
		{name: "-o output with a directory keploy does not know", layout: rhel9, says: "staged pem/new", setup: func(t *testing.T, st *p11Store) {
			st.stage = func(dir string) { _ = os.MkdirAll(filepath.Join(dir, "pem", "new"), 0o755) }
		}},
		{name: "a file under a hash link's name in the store", layout: fedora44, says: "is not a link", setup: func(t *testing.T, st *p11Store) {
			if err := os.WriteFile(filepath.Join(st.dirHash(), "0123abcd.0"), []byte("mine"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name+"/"+tc.layout.String(), func(t *testing.T) {
			newFakeTrustStore(t)
			st := newP11Store(t, tc.layout, 1, tc.opts...)
			if tc.setup != nil {
				tc.setup(t, st)
			}
			caStoreUpdateCmd = []string{st.wrapper}
			st.addCA(t, st.anchors, "keploy-mitm-"+testRunID+".crt", caCNPrefix+testRunID)
			before := st.layout0(t)
			logger, logs := observedLogger()

			if handled, err := swapP11KitTrust(context.Background(), logger, st.wrapper); handled {
				t.Fatalf("keploy stood in for a wrapper it does not know all of (err=%v)", err)
			}
			warned := false
			for _, e := range logs.FilterLevelExact(zapcore.WarnLevel).All() {
				if strings.Contains(fmt.Sprint(e.ContextMap()), tc.says) {
					warned = true
				}
			}
			if !warned {
				t.Errorf("no warning names %q: %v", tc.says, logs.All())
			}
			if after := st.layout0(t); !mapsEqual(before, after) {
				t.Error("the store was changed before keploy gave the wrapper the job")
			}

			err := refreshCaStore(context.Background(), zap.NewNop(), false)
			if ran, _ := os.ReadFile(st.wrapperLog); len(ran) == 0 {
				t.Error("the distro's wrapper was not run")
			}
			if err == nil {
				t.Error("the wrapper's failure was not reported")
			}
		})
	}
}

// TestSwapP11KitTrust_PutsInWhatTheWrappersOwnRunWrites: with -o, the output
// is the wrapper's own, whatever its script does to get it — here one more
// file than keploy knows of — and keploy puts all of it in place.
func TestSwapP11KitTrust_PutsInWhatTheWrappersOwnRunWrites(t *testing.T) {
	st := newP11Store(t, fedora44, 1)
	extra := []byte("the wrapper's own\n")
	st.stage = func(dir string) { _ = os.WriteFile(filepath.Join(dir, "pem", "extra.pem"), extra, 0o444) }
	if err := st.swap(t); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(st.extractedReal, "pem", "extra.pem")); !bytes.Equal(got, extra) {
		t.Error("a file the wrapper's run wrote was not put in place")
	}
}

// ---------------------------------------------------------------------------
// the swap
// ---------------------------------------------------------------------------

// TestSwapP11KitTrust_NoCertificateIsEverOutOfReach changes the anchors in the
// ways agents do — a CA added, a CA removed — and in the rare ways that move
// other CAs: a CA whose hashes collide with a distro root's and is read before
// it; two roots that share hashes and swap places; and two or three CAs that
// share a label (a renewed root keeps its subject), so that p11-kit names them
// X.pem, X.1.pem, X.2.pem and moves their contents between those names when
// one of them leaves or their order changes. After every single change to the
// live store everything trusted before and after must still be trusted; at
// the end the store must be what the distro's tool makes of the same anchors.
func TestSwapP11KitTrust_NoCertificateIsEverOutOfReach(t *testing.T) {
	type change struct {
		name string
		do   func(t *testing.T, st *p11Store)
	}
	addKeploy := func(t *testing.T, st *p11Store) {
		st.addCA(t, st.anchors, "keploy-mitm-"+testRunID+".crt", caCNPrefix+testRunID)
	}
	removeKeploy := func(t *testing.T, st *p11Store) {
		if err := os.Remove(filepath.Join(st.anchors, "keploy-mitm-"+testRunID+".crt")); err != nil {
			t.Fatal(err)
		}
	}
	collide := func(t *testing.T, st *p11Store) {
		// The anchors are read before the distro's roots, so this CA takes
		// index 0 of both of root A's hashes and pushes root A to index 1.
		root := st.cas(t)[0]
		st.setHashes(t, caCNPrefix+testRunID, root.hashes...)
	}
	sharedRoots := func(t *testing.T, st *p11Store) {
		cas := st.cas(t)
		st.setHashes(t, cas[2].cn, cas[1].hashes...)
	}
	flip := func(a, b string) func(t *testing.T, st *p11Store) {
		// Two roots change the order they are read in: each moves to the
		// other's index, and, if they share a label, to the other's file.
		return func(t *testing.T, st *p11Store) {
			pa, pb := filepath.Join(st.roots, a), filepath.Join(st.roots, b)
			tmp := filepath.Join(st.roots, "tmp")
			for _, mv := range [][2]string{{pa, tmp}, {pb, pa}, {tmp, pb}} {
				if err := os.Rename(mv[0], mv[1]); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	sameLabel := func(n int) func(t *testing.T, st *p11Store) {
		return func(t *testing.T, st *p11Store) {
			for i := 1; i <= n; i++ {
				st.addCA(t, st.roots, fmt.Sprintf("root-x%d.crt", i), "Example Corp Root")
			}
		}
	}
	retireFirstSameLabel := func(t *testing.T, st *p11Store) {
		if err := os.Remove(filepath.Join(st.roots, "root-x1.crt")); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name   string
		before []change // applied, then the store regenerated by the distro's tool
		swap   []change // applied, then the store swapped by keploy
	}{
		{name: "a CA is added", swap: []change{{"add", addKeploy}}},
		{name: "a CA is removed", before: []change{{"add", addKeploy}}, swap: []change{{"remove", removeKeploy}}},
		{name: "a CA that shares a root's hashes is added ahead of it", before: []change{{"collide", collide}},
			swap: []change{{"add", addKeploy}}},
		{name: "a CA that shares a root's hashes is removed from ahead of it", before: []change{{"collide", collide}, {"add", addKeploy}},
			swap: []change{{"remove", removeKeploy}}},
		{name: "two roots that share hashes swap places while a CA is added", before: []change{{"share", sharedRoots}},
			swap: []change{{"flip", flip("root-b.crt", "root-c.crt")}, {"add", addKeploy}}},
		{name: "of three CAs that share a label the first leaves while a CA is added", before: []change{{"same label", sameLabel(3)}},
			swap: []change{{"retire", retireFirstSameLabel}, {"add", addKeploy}}},
		{name: "two CAs that share a label swap places", before: []change{{"same label", sameLabel(2)}},
			swap: []change{{"flip", flip("root-x1.crt", "root-x2.crt")}}},
	} {
		for _, layout := range allLayouts {
			t.Run(tc.name+"/"+layout.String(), func(t *testing.T) {
				st := newP11Store(t, layout, 3)
				for _, c := range tc.before {
					c.do(t, st)
				}
				if len(tc.before) > 0 {
					st.regenerate(t)
				}
				before := st.cas(t)
				for _, c := range tc.swap {
					c.do(t, st)
				}
				changes := st.watchSwap(t, intersect(before, st.cas(t)))

				if err := st.swap(t); err != nil {
					t.Fatalf("the swap failed: %v", err)
				}
				if *changes == 0 {
					t.Fatal("the swap changed nothing")
				}
				st.expectDistroLayout(t)
				if got, _ := os.ReadFile(st.wrapperLog); len(got) != 0 {
					t.Error("the distro's update-ca-trust was run")
				}
			})
		}
	}
}

// TestSwapP11KitTrust_TheSwapAfterOneThatDiedKeepsEveryCAInReach: a swap
// that dies part way — here after its n-th change, for every n, in the case
// that makes holds: of three CAs that share a label the first leaves — leaves
// holds behind, and links to them, and maybe a CA that only a hold keeps in
// reach. The next swap must keep every CA trusted before and after in reach at
// every step: it keeps a dead swap's holds until its own chain rewrite no
// longer needs them, and links its own holds at the end of what OpenSSL can
// reach, not behind a link that does not resolve.
func TestSwapP11KitTrust_TheSwapAfterOneThatDiedKeepsEveryCAInReach(t *testing.T) {
	for _, layout := range []p11Layout{fedora44, rhel9, al2023} {
		for n := 1; ; n++ {
			finished := false
			t.Run(fmt.Sprintf("%s/dies-after-%d", layout, n), func(t *testing.T) {
				st := newP11Store(t, layout, 2)
				for i := 1; i <= 3; i++ {
					st.addCA(t, st.roots, fmt.Sprintf("root-x%d.crt", i), "Example Corp Root")
				}
				st.regenerate(t)
				before := st.cas(t)
				if err := os.Remove(filepath.Join(st.roots, "root-x1.crt")); err != nil {
					t.Fatal(err)
				}
				st.addCA(t, st.anchors, "keploy-mitm-"+testRunID+".crt", caCNPrefix+testRunID)

				steps, died := 0, false
				p11kitSwapStep = func(string) {
					if steps++; steps == n {
						died = true
						runtime.Goexit()
					}
				}
				done := make(chan struct{})
				go func() {
					defer close(done)
					_, _ = swapP11KitTrust(context.Background(), zap.NewNop(), st.wrapper)
				}()
				<-done
				p11kitSwapStep = nil
				if !died {
					finished = true
					return
				}

				st.watchSwap(t, intersect(before, st.cas(t)))
				if err := st.swap(t); err != nil {
					t.Fatalf("the swap after one that died failed: %v", err)
				}
				p11kitSwapStep = nil
				st.expectDistroLayout(t)
			})
			if finished || t.Failed() {
				break
			}
		}
	}
}

// TestSwapP11KitTrust_ADanglingLinkEndsTheChainItIsIn: OpenSSL stops at the
// first link of a chain that does not resolve, as at one that is missing. So a
// hold linked after such a link — one whose file somebody took away — keeps
// nothing in reach, and the swap links its holds at the first link that does
// not resolve instead, here and in the certs directory.
func TestSwapP11KitTrust_ADanglingLinkEndsTheChainItIsIn(t *testing.T) {
	for _, layout := range []p11Layout{fedora44, rhel9} {
		t.Run(layout.String(), func(t *testing.T) {
			st := newP11Store(t, layout, 2)
			for i := 1; i <= 3; i++ {
				st.addCA(t, st.roots, fmt.Sprintf("root-x%d.crt", i), "Example Corp Root")
			}
			st.regenerate(t)
			var corp fakeCA
			for _, c := range st.cas(t) {
				if c.cn == "Example Corp Root" {
					corp = c
				}
			}
			dirs := []string{st.dirHash()}
			if layout.certsLinked() {
				dirs = append(dirs, st.certsDir)
			}
			for _, dir := range dirs {
				for _, h := range corp.hashes {
					if err := os.Symlink(filepath.Join(dir, "gone.pem"), filepath.Join(dir, h+".3")); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := st.cas(t)
			if err := os.Remove(filepath.Join(st.roots, "root-x1.crt")); err != nil {
				t.Fatal(err)
			}
			st.watchSwap(t, intersect(before, st.cas(t)))
			if err := st.swap(t); err != nil {
				t.Fatal(err)
			}
			p11kitSwapStep = nil
			st.expectDistroLayout(t)
		})
	}
}

// fsEvent is one inotify event: what happened to name in dir.
type fsEvent struct {
	dir, name string
	mask      uint32
}

// watchDirs starts recording every name created, deleted, written or moved in
// the given directories.
func watchDirs(t *testing.T, dirs ...string) func() []fsEvent {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	wds := map[int32]string{}
	for _, d := range dirs {
		wd, err := unix.InotifyAddWatch(fd, d, unix.IN_CREATE|unix.IN_DELETE|unix.IN_MODIFY|unix.IN_CLOSE_WRITE|unix.IN_MOVED_FROM|unix.IN_MOVED_TO)
		if err != nil {
			t.Fatal(err)
		}
		wds[int32(wd)] = d
	}
	// Everything the swap did is queued by the time it returns: reading
	// until the queue is empty reads all of it.
	return func() []fsEvent {
		var events []fsEvent
		buf := make([]byte, 1<<16)
		for {
			n, err := unix.Read(fd, buf)
			if errors.Is(err, unix.EAGAIN) {
				return events
			}
			if err != nil {
				t.Fatal(err)
			}
			for off := 0; off+unix.SizeofInotifyEvent <= n; {
				wd := int32(binary.NativeEndian.Uint32(buf[off:]))
				mask := binary.NativeEndian.Uint32(buf[off+4:])
				nameLen := int(binary.NativeEndian.Uint32(buf[off+12:]))
				name := string(bytes.TrimRight(buf[off+unix.SizeofInotifyEvent:off+unix.SizeofInotifyEvent+nameLen], "\x00"))
				if mask&unix.IN_Q_OVERFLOW != 0 {
					t.Fatal("the inotify queue overflowed: the swap's changes could not all be checked")
				}
				events = append(events, fsEvent{dir: wds[wd], name: name, mask: mask})
				off += unix.SizeofInotifyEvent + nameLen
			}
		}
	}
}

func dirNames(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	return names
}

// TestSwapP11KitTrust_EveryNameIsReplacedOnlyByARename is what the no-gap
// rule rests on, checked on the primitive: a name that is in the store both
// before and after the swap is only ever replaced by renaming another file
// over it — never removed first, never created anew, never truncated or
// written in place. The swap adds a CA that collides with a root (so hash
// links are replaced), with two CAs that share a label trading places (so
// certificate files are), and replaces every bundle. The kernel reports every
// change to the directories (inotify), so a gap of any length is seen.
func TestSwapP11KitTrust_EveryNameIsReplacedOnlyByARename(t *testing.T) {
	st := newP11Store(t, fedora44, 3)
	st.addCA(t, st.roots, "root-x1.crt", "Example Corp Root")
	st.addCA(t, st.roots, "root-x2.crt", "Example Corp Root")
	st.setHashes(t, caCNPrefix+testRunID, st.cas(t)[0].hashes...)
	st.regenerate(t)
	st.addCA(t, st.anchors, "keploy-mitm-"+testRunID+".crt", caCNPrefix+testRunID)
	for _, mv := range [][2]string{{"root-x1.crt", "tmp"}, {"root-x2.crt", "root-x1.crt"}, {"tmp", "root-x2.crt"}} {
		if err := os.Rename(filepath.Join(st.roots, mv[0]), filepath.Join(st.roots, mv[1])); err != nil {
			t.Fatal(err)
		}
	}

	var dirs []string
	for _, d := range []string{"openssl", "pem", "java", "edk2", "pem/directory-hash"} {
		dirs = append(dirs, filepath.Join(st.extractedReal, d))
	}
	dirs = append(dirs, st.certsDir)
	before := map[string]map[string]bool{}
	for _, d := range dirs {
		before[d] = dirNames(t, d)
	}
	events := watchDirs(t, dirs...)

	if err := st.swap(t); err != nil {
		t.Fatal(err)
	}

	replaced := 0
	for _, e := range events() {
		if strings.HasPrefix(e.name, p11kitSwapPrefix) || !before[e.dir][e.name] || !dirNames(t, e.dir)[e.name] {
			continue
		}
		switch {
		case e.mask&unix.IN_MOVED_TO != 0:
			replaced++
		case e.mask&(unix.IN_DELETE|unix.IN_CREATE|unix.IN_MODIFY|unix.IN_CLOSE_WRITE) != 0:
			t.Errorf("%s in %s was not replaced by a rename (inotify mask %#x): for a moment it was missing or partly written",
				e.name, e.dir, e.mask)
		}
	}
	if replaced == 0 {
		t.Fatal("the swap replaced no name that was there before and after: the test proves nothing")
	}
	st.expectDistroLayout(t)
}

// TestSwapP11KitTrust_RefillsADefaultDirectoryAWrapperRunLeftEmpty: Fedora's
// wrapper deletes every link in /etc/pki/tls/certs before it regenerates, so a
// run of it that was cut short (keploy used to kill it with the run's context)
// leaves the directory empty. A wrapper that makes those links is one whose
// store keploy keeps them in, empty or not.
func TestSwapP11KitTrust_RefillsADefaultDirectoryAWrapperRunLeftEmpty(t *testing.T) {
	st := newP11Store(t, fedora44, 3)
	entries, err := os.ReadDir(st.certsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(st.certsDir, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.swap(t); err != nil {
		t.Fatal(err)
	}
	st.expectDistroLayout(t)
}

// TestSwapP11KitTrust_LeavesTheCertsDirectoryAloneWhereTheDistroDoes: the
// wrappers of RHEL 9 and Amazon Linux 2023 put nothing in /etc/pki/tls/certs
// and take nothing out, so neither does keploy — not even links into
// directory-hash it finds there, which some earlier tool or person made.
func TestSwapP11KitTrust_LeavesTheCertsDirectoryAloneWhereTheDistroDoes(t *testing.T) {
	for _, layout := range []p11Layout{rhel9, al2023} {
		t.Run(layout.String(), func(t *testing.T) {
			st := newP11Store(t, layout, 2)
			root := st.cas(t)[0]
			old := filepath.Join(st.certsDir, root.hashes[0]+".0")
			if err := os.Symlink(filepath.Join(st.dirHash(), fakeLabel(root.cn)+".pem"), old); err != nil {
				t.Fatal(err)
			}
			before := layoutOf(t, st.root, st.certsDir)
			st.addCA(t, st.anchors, "keploy-mitm-"+testRunID+".crt", caCNPrefix+testRunID)
			if err := st.swap(t); err != nil {
				t.Fatal(err)
			}
			if after := layoutOf(t, st.root, st.certsDir); !mapsEqual(before, after) {
				t.Errorf("the swap changed a certs directory its distro's wrapper leaves alone:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

// TestSwapP11KitTrust_PointsLinksWhereTheWrapperDoes: the wrapper links each
// name in /etc/pki/tls/certs to the file its directory-hash link resolves to
// ("readlink -f"), so on a machine where the store is reached through a
// symlink those links name the real path.
func TestSwapP11KitTrust_PointsLinksWhereTheWrapperDoes(t *testing.T) {
	st := newP11Store(t, fedora44, 2, behindSymlink)
	st.addCA(t, st.anchors, "keploy-mitm-"+testRunID+".crt", caCNPrefix+testRunID)
	if err := st.swap(t); err != nil {
		t.Fatal(err)
	}
	st.expectDistroLayout(t)
}

// TestSwapP11KitTrust_RestoresACompatibilityLinkTheWrapperMakes: RHEL 9's and
// Amazon Linux's wrappers add two links to directory-hash after extracting it;
// a run of it cut short in between leaves them missing, and the swap, which
// stands in for the wrapper, adds them back.
func TestSwapP11KitTrust_RestoresACompatibilityLinkTheWrapperMakes(t *testing.T) {
	for _, layout := range []p11Layout{rhel9, al2023} {
		t.Run(layout.String(), func(t *testing.T) {
			st := newP11Store(t, layout, 2)
			if err := os.Remove(filepath.Join(st.dirHash(), "ca-bundle.crt")); err != nil {
				t.Fatal(err)
			}
			if err := st.swap(t); err != nil {
				t.Fatal(err)
			}
			st.expectDistroLayout(t)
		})
	}
}

// TestSwapP11KitTrust_RemovesWhatTheWrappersOutputDoesNotHave: p11-kit's own
// overwrite of directory-hash removes every entry it did not write — a stray
// file, a compatibility link the wrapper no longer makes — and the wrapper
// adds back only its own links. The swap leaves directory-hash as that does.
func TestSwapP11KitTrust_RemovesWhatTheWrappersOutputDoesNotHave(t *testing.T) {
	for _, layout := range []p11Layout{fedora44, rhel9, al2023} {
		t.Run(layout.String(), func(t *testing.T) {
			st := newP11Store(t, layout, 2)
			if err := os.WriteFile(filepath.Join(st.dirHash(), "stray.crt"), testCertPEM(t), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../tls-ca-bundle.pem", filepath.Join(st.dirHash(), "old-compat.crt")); err != nil {
				t.Fatal(err)
			}
			if err := st.swap(t); err != nil {
				t.Fatal(err)
			}
			st.expectDistroLayout(t)
		})
	}
}

// TestSwapP11KitTrust_AFailedExtractionChangesNothing: the staging directory
// is filled before anything is put in place, so a failure there leaves the
// store exactly as it was — not half-regenerated, as the wrapper leaves it —
// and nothing of the attempt behind; and the job goes to the wrapper, which
// fails the same way or works where keploy's copy of its work did not.
func TestSwapP11KitTrust_AFailedExtractionChangesNothing(t *testing.T) {
	for _, layout := range []p11Layout{fedora44, al2023} {
		t.Run(layout.String(), func(t *testing.T) {
			st := newP11Store(t, layout, 3)
			st.addCA(t, st.anchors, "keploy-mitm-"+testRunID+".crt", caCNPrefix+testRunID)
			before := st.layout0(t)
			st.failFormat = "pem-directory-hash"
			logger, logs := observedLogger()

			handled, err := swapP11KitTrust(context.Background(), logger, st.wrapper)
			if handled || err != nil {
				t.Fatalf("a failed extraction was not handed to the wrapper (handled=%v, err=%v)", handled, err)
			}
			if !strings.Contains(fmt.Sprint(logs.All()), "couldn't extract pem-directory-hash") {
				t.Errorf("the warning does not say what failed: %v", logs.All())
			}
			if after := st.layout0(t); !mapsEqual(before, after) {
				t.Fatal("a failed swap changed the store")
			}
			for name := range dirNames(t, st.extractedReal) {
				if strings.HasPrefix(name, p11kitSwapPrefix) {
					t.Errorf("a failed swap left %s", name)
				}
			}
		})
	}
}

// TestReadP11KitLayout_WithOutputTheWrapperDoesItsOwnWork: with -o the
// wrapper writes its own output, so what keploy would refuse to do itself — an
// output it does not know, the same output twice, an extraction through a
// function — is the wrapper's business, and keploy swaps in whatever it wrote.
func TestReadP11KitLayout_WithOutputTheWrapperDoesItsOwnWork(t *testing.T) {
	st := newP11Store(t, fedora44, 1, withWrapperLines(
		`/usr/bin/trust extract --format=pem-bundle --filter=ca-anchors --overwrite --purpose server-auth "$DEST/pem/new.pem"`,
		`if false; then`,
		`/usr/bin/trust extract --format=pem-bundle --filter=ca-anchors --overwrite --comment --purpose server-auth "$DEST/pem/tls-ca-bundle.pem"`,
		`fi`,
		`ex --format=pem-bundle --filter=ca-anchors --overwrite --comment --purpose email "$DEST/pem/email-ca-bundle.pem"`,
	))
	l, err := readP11KitLayout(st.wrapper)
	if err != nil {
		t.Fatal(err)
	}
	if !l.output || len(l.unknown) != 0 {
		t.Errorf("a wrapper with -o was not taken on (output=%v, unknown=%q)", l.output, l.unknown)
	}
}

// TestSwapP11KitTrust_ClearsWhatADeadSwapLeft: an agent killed mid-swap leaves
// its staging directory — read-only, as p11-kit leaves what it writes — and
// maybe a replacement link it had not renamed into place, or a hold and the
// link to it past the end of a chain. The next swap, under the same lock,
// removes them.
func TestSwapP11KitTrust_ClearsWhatADeadSwapLeft(t *testing.T) {
	st := newP11Store(t, fedora44, 2)
	dead := filepath.Join(st.extractedReal, p11kitSwapPrefix+"dead")
	if err := os.MkdirAll(filepath.Join(dead, "pem_directory-hash"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dead, "pem_directory-hash"), 0o555); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{st.certsDir, st.dirHash()} {
		if err := os.Symlink("nowhere", filepath.Join(dir, p11kitSwapPrefix+"1-1")); err != nil {
			t.Fatal(err)
		}
	}
	root := st.cas(t)[0]
	hold := p11kitSwapPrefix + "hold-1-2"
	if err := os.Link(filepath.Join(st.dirHash(), fakeLabel(root.cn)+".pem"), filepath.Join(st.dirHash(), hold)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(hold, filepath.Join(st.dirHash(), root.hashes[0]+".1")); err != nil {
		t.Fatal(err)
	}
	if err := st.swap(t); err != nil {
		t.Fatal(err)
	}
	st.expectDistroLayout(t)
}

// TestSwapHashChain_RefusesToReplaceAFile: a regular file under a hash name is
// somebody else's; keploy does not replace it.
func TestSwapHashChain_RefusesToReplaceAFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "0123abcd.0"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := swapHashChain(dir, "0123abcd", map[int]string{}, []string{"/x/a.pem"}); err == nil {
		t.Error("replaced a regular file with a link")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "0123abcd.0")); string(got) != "mine" {
		t.Error("the file was changed")
	}
}

// TestSwapHashChain_LeavesAFileInAChainAlone: removing the links past a
// chain's end removes links only, not a regular file that happens to sit at
// one of those names.
func TestSwapHashChain_LeavesAFileInAChainAlone(t *testing.T) {
	dir := t.TempDir()
	for i, target := range []string{"a.pem", "", "b.pem"} {
		name := filepath.Join(dir, "0123abcd."+strconv.Itoa(i))
		var err error
		if target == "" {
			err = os.WriteFile(name, []byte("mine"), 0o644)
		} else {
			err = os.Symlink(target, name)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := swapHashChain(dir, "0123abcd", map[int]string{0: "a.pem", 2: "b.pem"}, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "0123abcd.1")); string(got) != "mine" {
		t.Error("a regular file in the chain was removed")
	}
	for _, i := range []string{"0", "2"} {
		if exists(filepath.Join(dir, "0123abcd."+i)) {
			t.Errorf("the link 0123abcd.%s of a chain that is gone was left", i)
		}
	}
}

// ---------------------------------------------------------------------------
// the install on a p11-kit store
// ---------------------------------------------------------------------------

// TestNativeInstall_OnAP11KitStoreNeverRunsTheWrapper: on Fedora, RHEL and
// Amazon Linux the install and the teardown regenerate through the swap, not
// update-ca-trust: the CA is trusted through every output while the run lasts
// — java/cacerts included, which the JDK reads and keploy no longer writes
// with keytool — and once it has stopped the store is what it was before it
// started.
func TestNativeInstall_OnAP11KitStoreNeverRunsTheWrapper(t *testing.T) {
	for _, layout := range allLayouts {
		t.Run(layout.String(), func(t *testing.T) {
			s := newFakeTrustStore(t)
			st := newP11Store(t, layout, 3)
			caStorePath = []string{st.anchors + "/"}
			caStoreUpdateCmd = []string{st.wrapper}
			before := st.layout0(t)

			ca := s.mustInstall(t)
			if got, _ := os.ReadFile(st.wrapperLog); len(got) != 0 {
				t.Fatal("the install ran the distro's update-ca-trust")
			}
			var mine fakeCA
			for _, c := range st.cas(t) {
				if c.cn == caCNPrefix+ca.runID {
					mine = c
				}
			}
			if mine.der == nil {
				t.Fatal("the install wrote no anchor")
			}
			for _, o := range layout.outputs() {
				if !o.dir && !bundleHolds(filepath.Join(st.extractedReal, o.rel), mine) {
					t.Errorf("the CA is not in %s", o.rel)
				}
			}
			if layout != rhel8 && !openSSLFinds(st.dirHash(), mine) {
				t.Error("OpenSSL cannot find the CA in directory-hash")
			}
			if layout.certsLinked() && !openSSLFinds(st.certsDir, mine) {
				t.Error("OpenSSL cannot find the CA in its default certificate directory")
			}
			st.expectDistroLayout(t)

			s.teardown(t)
			if got, _ := os.ReadFile(st.wrapperLog); len(got) != 0 {
				t.Fatal("the teardown ran the distro's update-ca-trust")
			}
			if after := st.layout0(t); !mapsEqual(before, after) {
				t.Errorf("the teardown did not leave the store as it found it")
			}
		})
	}
}

// TestNativeInstall_LeavesAGeneratedJavaKeystoreToTheRegeneration: where the
// JDK's cacerts is a p11-kit output (/etc/pki/java/cacerts →
// extracted/java/cacerts), keytool would rewrite it in place — every JVM
// starting meanwhile reads a torn truststore — only for the regeneration to
// replace it. So keploy leaves it to the regeneration, which puts the CA in
// and takes it out again.
func TestNativeInstall_LeavesAGeneratedJavaKeystoreToTheRegeneration(t *testing.T) {
	s := newFakeTrustStore(t)
	st := newP11Store(t, fedora44, 2)
	caStorePath = []string{st.anchors + "/"}
	caStoreUpdateCmd = []string{st.wrapper}
	cacerts := filepath.Join(s.javaHome, "lib", "security", "cacerts")
	if err := os.Remove(cacerts); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(st.extracted, "java", "cacerts"), cacerts); err != nil {
		t.Fatal(err)
	}

	ca := s.mustInstall(t)
	if n := s.countLogLines(t, s.keytoolLog, "-import"); n != 0 {
		t.Errorf("keytool imported into a generated keystore %d times", n)
	}
	der := ca.der
	data, err := os.ReadFile(cacerts)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, der) {
		t.Error("the regeneration did not put the CA in the JDK's keystore")
	}

	s.teardown(t)
	if n := s.countLogLines(t, s.keytoolLog, "-delete"); n != 0 {
		t.Errorf("keytool deleted from a generated keystore %d times", n)
	}
	if data, _ := os.ReadFile(cacerts); bytes.Contains(data, der) {
		t.Error("the teardown's regeneration did not take the CA out of the JDK's keystore")
	}
}

// observedLogger returns a logger and everything it logs.
func observedLogger() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)
	return zap.New(core), logs
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}
