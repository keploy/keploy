//go:build linux

package tls

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"go.uber.org/zap"
)

// The Fedora / RHEL trust store, and how keploy changes it without a gap.
//
// On Fedora, RHEL and their rebuilds an anchor is a file in
// /etc/pki/ca-trust/source/anchors, and what applications read is generated
// from the anchors by p11-kit ("trust extract") into /etc/pki/ca-trust/extracted:
//
//	openssl/ca-bundle.trust.crt   OpenSSL's "TRUSTED CERTIFICATE" bundle
//	pem/tls-ca-bundle.pem         the TLS bundle: curl's CA file, the first file
//	                              Go finds, Python requests' certifi, and
//	                              /etc/pki/tls/cert.pem where that still exists
//	pem/email-ca-bundle.pem       S/MIME
//	pem/objsign-ca-bundle.pem     code signing
//	java/cacerts                  the JDK's cacerts, through /etc/pki/java/cacerts
//	edk2/cacerts.bin              firmware
//	pem/directory-hash/           one <label>.pem per CA and <hash>.<n> links to
//	                              them: an OpenSSL CApath (RHEL 9, 10, Fedora)
//
// and, on RHEL 10 and Fedora, one absolute link in /etc/pki/tls/certs for each
// hash link in directory-hash: OpenSSL's default certificate directory. On
// Fedora 44 there is no /etc/pki/tls/cert.pem, so that directory is the only
// default source of OpenSSL and of everything that uses its defaults — Python's
// ssl module, Node (Fedora builds it to use them), Ruby, PHP, openssl itself.
//
// The distro's wrapper, update-ca-trust, deletes every link in /etc/pki/tls/certs
// before it extracts anything and recreates them after the last extraction:
// 1.6 to 3.9 s on every run with no certificate at all in OpenSSL's default
// directory. p11-kit's own --overwrite of directory-hash unlinks and recreates
// each hash link as well. With agents starting and stopping around it, an
// application that verified with OpenSSL's defaults failed whenever any other
// agent changed the store.
//
// So keploy does not run the wrapper on the live store. Where the wrapper can
// write its output somewhere else ("update-ca-trust extract -o DIR": Fedora,
// RHEL 9 and 10), keploy runs it into a staging directory, which gives exactly
// what a live run would write there, and then puts that output in place. Where
// it cannot (RHEL 8, Amazon Linux 2023), keploy reads the wrapper's extraction
// lines and runs them into the staging directory itself. Either way the
// staging directory is swapped in with each change atomic and ordered so that
// a certificate that is trusted both before and after is trusted throughout:
//
//   - a file output is renamed over the old one;
//   - directory-hash cannot be renamed over (rename(2) needs an empty target
//     directory, and on overlayfs a directory of the image cannot be renamed at
//     all, nor swapped with RENAME_EXCHANGE: both are EXDEV), so it is changed
//     entry by entry (swapDirectoryHash);
//   - the links in /etc/pki/tls/certs, which the live run of the wrapper fills
//     and the -o run does not, are rewritten the same way, to point where the
//     wrapper points them.
//
// What is left is what the wrapper leaves, entry for entry, so the distro's own
// update-ca-trust, run at any time later, finds the store as it left it. That
// rests on keploy knowing exactly what the wrapper does (readP11KitLayout):
// anything in it keploy does not recognise, and anything in its output keploy
// does not know how to swap, makes keploy run the wrapper as it is, gap and
// all, with a warning that names what it did not recognise.

// p11kitSwapStep, when set, is called after every single change the swap makes
// to the live store. It is for tests, which check after each one that nothing
// trusted before and after the swap is missing.
var p11kitSwapStep func(change string)

// p11kitRunExtract runs the command that fills the staging directory: the
// wrapper with -o, or one trust extract. A variable so that tests can play
// p11-kit and the wrapper in-process, against a store of their own.
var p11kitRunExtract = runStoreCommand

// p11kitSwapPrefix starts the name of everything the swap creates that is not
// the store's own: the staging directory, a replacement link before it is
// renamed into place, a hold (swapDirectoryHash). Anything by that name found
// later is a dead swap's.
const (
	p11kitSwapPrefix = ".keploy-swap-"
	p11kitHoldPrefix = p11kitSwapPrefix + "hold-"
)

// p11kitDirs are the directories of a p11-kit store's outputs, relative to its
// DEST; dirHashRel is the one that is itself an output.
var p11kitDirs = map[string]bool{"openssl": true, "pem": true, "java": true, "edk2": true}

const dirHashRel = "pem/directory-hash"

// p11kitKnownOutputs are the outputs keploy knows how to swap, each with the
// trust extract arguments that produce it, as normalizeExtractArgs leaves them.
// They are those of every wrapper seen: RHEL 8 and Amazon Linux 2023
// ("p11-kit extract"), RHEL 9 and 10, Fedora 44 ("trust extract"). RHEL 8 has
// no directory-hash.
var p11kitKnownOutputs = map[string]struct {
	dir  bool
	args string
}{
	"openssl/ca-bundle.trust.crt": {args: "--comment --filter=certificates --format=openssl-bundle"},
	"pem/tls-ca-bundle.pem":       {args: "--comment --filter=ca-anchors --format=pem-bundle --purpose=server-auth"},
	"pem/email-ca-bundle.pem":     {args: "--comment --filter=ca-anchors --format=pem-bundle --purpose=email"},
	"pem/objsign-ca-bundle.pem":   {args: "--comment --filter=ca-anchors --format=pem-bundle --purpose=code-signing"},
	"java/cacerts":                {args: "--filter=ca-anchors --format=java-cacerts --purpose=server-auth"},
	"edk2/cacerts.bin":            {args: "--filter=ca-anchors --format=edk2-cacerts --purpose=server-auth"},
	dirHashRel:                    {dir: true, args: "--filter=ca-anchors --format=pem-directory-hash --purpose=server-auth"},
}

// p11kitKnownLinks are the compatibility links a wrapper adds into the store
// that keploy knows (RHEL 9, Amazon Linux 2023): Debian's and the old RHEL
// bundle names, inside directory-hash.
var p11kitKnownLinks = map[string]string{
	"pem/directory-hash/ca-certificates.crt": "../tls-ca-bundle.pem",
	"pem/directory-hash/ca-bundle.crt":       "../tls-ca-bundle.pem",
}

// p11kitLayout is what a distro's update-ca-trust does, as read from the
// script itself.
type p11kitLayout struct {
	wrapper   string
	dest      string         // $DEST: /etc/pki/ca-trust/extracted
	output    bool           // it can write its output elsewhere: "extract -o DIR"
	certsDir  string         // $DEST_CERTS, which its live run fills with links into directory-hash; "" where it does not
	outputs   []p11kitOutput // without -o: the outputs it extracts, in its order
	links     []p11kitLink   // without -o: the compatibility links it adds
	generated []string       // every output it extracts that keploy can read off a line, relative to dest
	unknown   []string       // every line keploy does not know the effect of
}

// p11kitOutput is one output the wrapper extracts, and how.
type p11kitOutput struct {
	rel  string // under dest
	dir  bool
	tool string   // the wrapper's tool: /usr/bin/trust, or /usr/bin/p11-kit
	args []string // after "extract", without --overwrite and the destination
}

// p11kitLink is a compatibility link the wrapper adds into the store.
type p11kitLink struct {
	rel, target string
}

var (
	wrapperDestLine      = regexp.MustCompile(`^DEST=(["']?)(/[^"'\s]+)(["']?)$`)
	wrapperDestCertsLine = regexp.MustCompile(`^DEST_CERTS=(["']?)(/[^"'\s]+)(["']?)$`)
	wrapperMentionsDest  = regexp.MustCompile(`\$\{?DEST\b`)
	wrapperMentionsCerts = regexp.MustCompile(`\$\{?DEST_CERTS\b`)
	wrapperAssigns       = regexp.MustCompile(`(^|[^A-Za-z0-9_])DEST(_CERTS)?=`)
)

// The lines of the blocks a wrapper runs only on the live store
// ("if [ -z "$USER_DEST" ]; then ... fi"): emptying $DEST_CERTS of its hash
// links, and filling it with an absolute link per hash link of directory-hash.
// keploy does that work itself (swapDirectoryHash), so these are the only lines
// such a block may hold.
var (
	liveCertsEmptied = []string{
		`find "$DEST_CERTS" -type l -regextype posix-extended \`,
		`-regex '.*/[0-9a-f]{8}\.[0-9]+' -exec rm -f {} \;`,
	}
	liveCertsFilled = [][]string{
		{`find "$DEST/pem/directory-hash" -type l -regextype posix-extended \`},
		{`-regex '.*/[0-9a-f]{8}\.[0-9]+' | while IFS= read -r link; do`, `-regex '.*/[0-9a-f]{8}\.[0-9]+' | while read link; do`},
		{`target=$(readlink -f "$link")`},
		{`new_link="$DEST_CERTS/$(basename "$link")"`},
		{`ln -s "$target" "$new_link"`},
		{`done`},
	}
)

// readP11KitLayout reads a distro's update-ca-trust: the store it writes
// ($DEST), whether it can write its output elsewhere (-o), whether its live run
// fills $DEST_CERTS with links into directory-hash, and — for a wrapper without
// -o, whose work keploy then does itself — every "trust extract" (or "p11-kit
// extract") it runs into the store and every link it adds into it.
//
// It is strict: a line keploy cannot account for is in unknown. That is any
// assignment of DEST or DEST_CERTS but one plain one each; any line that names
// the store's path itself; any line of a live-only block but the ones that
// empty and fill $DEST_CERTS, and one of those without the other; any other
// use of $DEST_CERTS; and, without -o, any line that uses $DEST and is not a
// known extraction (with its known arguments, once) or a known link.
func readP11KitLayout(wrapper string) (*p11kitLayout, error) {
	script, err := os.ReadFile(wrapper)
	if err != nil {
		return nil, err
	}
	l := &p11kitLayout{wrapper: wrapper}
	lines := strings.Split(string(script), "\n")
	// Whether it can write its output elsewhere decides how strictly the rest
	// is read, so it is read first: the -o option's arm, and DEST taking the
	// option's value.
	userDest, outputArm := false, false
	for _, raw := range lines {
		switch strings.Join(strings.Fields(raw), " ") {
		case "DEST=$USER_DEST":
			userDest = true
		case `"-o"|"--output")`:
			outputArm = true
		}
	}
	l.output = userDest && outputArm
	var (
		dests, certsDirs int
		emptied          = map[string]bool{}
		filled           = map[int]bool{}
		block            string // "live" inside a live-only block, "compat" inside Fedora's --rhbz2387674 block
		depth            int    // the if-nesting depth inside that block
		seenOutputs      = map[string]bool{}
		seenLinks        = map[string]bool{}
		unknown          = func(line string) { l.unknown = append(l.unknown, line) }
	)
	for _, raw := range lines {
		line := strings.Join(strings.Fields(raw), " ")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if block != "" {
			switch {
			case strings.HasPrefix(line, "if "):
				depth++
			case line == "fi" || strings.HasPrefix(line, "fi;") || strings.HasPrefix(line, "fi "):
				depth--
				if depth == 0 {
					block = ""
					continue
				}
			}
			// The compatibility block runs only with --rhbz2387674, which
			// neither keploy nor a plain run of the wrapper passes.
			if block == "live" && !liveOnlyLine(line, emptied, filled) {
				unknown(line)
			}
			continue
		}
		switch line {
		case `if [ -z "$USER_DEST" ]; then`:
			block, depth = "live", 1
			continue
		case `if [ -n "$compat" ]; then`:
			block, depth = "compat", 1
			continue
		}
		if m := wrapperDestLine.FindStringSubmatch(line); m != nil && m[1] == m[3] {
			dests++
			l.dest = m[2]
			continue
		}
		if m := wrapperDestCertsLine.FindStringSubmatch(line); m != nil && m[1] == m[3] {
			certsDirs++
			l.certsDir = m[2]
			continue
		}
		switch {
		case line == "DEST=$USER_DEST" || line == `"-o"|"--output")`:
			continue
		case wrapperAssigns.MatchString(line):
			// Any other assignment: exported, quoted otherwise, computed.
			unknown(line)
			continue
		case wrapperMentionsCerts.MatchString(line):
			unknown(line)
			continue
		}
		f := strings.Fields(line)
		isExtract := len(f) >= 3 && (filepath.Base(f[0]) == "trust" || filepath.Base(f[0]) == "p11-kit") && f[1] == "extract"
		rel, intoStore := "", false
		if isExtract {
			rel, intoStore = underDest(unquote(f[len(f)-1]))
			if intoStore {
				l.generated = append(l.generated, rel)
			}
		}
		if !wrapperMentionsDest.MatchString(line) || l.output {
			// With -o every use of the store is the wrapper's own, run into
			// the staging directory: keploy sees its result there.
			continue
		}
		switch {
		case isExtract && intoStore:
			args := normalizeExtractArgs(f[2 : len(f)-1])
			known, isKnown := p11kitKnownOutputs[rel]
			if !isKnown || known.args != args || seenOutputs[rel] {
				unknown(line)
				continue
			}
			seenOutputs[rel] = true
			l.outputs = append(l.outputs, p11kitOutput{rel: rel, dir: known.dir, tool: f[0], args: strings.Fields(args)})
		case len(f) == 4 && filepath.Base(f[0]) == "ln" && f[1] == "-s":
			lrel, ok := underDest(unquote(f[3]))
			if target := unquote(f[2]); ok && p11kitKnownLinks[lrel] == target && !seenLinks[lrel] {
				seenLinks[lrel] = true
				l.links = append(l.links, p11kitLink{rel: lrel, target: target})
				continue
			}
			unknown(line)
		default:
			// Without -o every use of the store must be one keploy does
			// itself.
			unknown(line)
		}
	}
	if l.output {
		// The wrapper writes the outputs; what keploy read off its lines is
		// only what systemTrustGenerates needs.
		l.outputs, l.links = nil, nil
	}
	switch {
	case dests != 1:
		return nil, fmt.Errorf("%s does not set DEST once (it sets it %d times)", wrapper, dests)
	case certsDirs > 1:
		unknown(fmt.Sprintf("DEST_CERTS is set %d times", certsDirs))
	}
	if l.dest != "" {
		for _, raw := range lines {
			line := strings.TrimSpace(raw)
			if !strings.HasPrefix(line, "#") && strings.Contains(line, l.dest) && !wrapperDestLine.MatchString(line) {
				unknown(line)
			}
		}
	}
	certsEmptied := len(emptied) == len(liveCertsEmptied)
	certsFilled := len(filled) == len(liveCertsFilled)
	switch {
	case l.certsDir != "" && certsEmptied && certsFilled:
	case l.certsDir == "" && len(emptied) == 0 && len(filled) == 0:
	default:
		unknown(fmt.Sprintf("DEST_CERTS=%q is not emptied and filled as keploy knows (emptied: %v, filled: %v)", l.certsDir, certsEmptied, certsFilled))
		l.certsDir = ""
	}
	if !l.output && len(l.outputs) == 0 && len(l.unknown) == 0 {
		return nil, fmt.Errorf("%s extracts nothing keploy can find", wrapper)
	}
	return l, nil
}

// liveOnlyLine reports whether line is one of those a live-only block may
// hold, and notes which.
func liveOnlyLine(line string, emptied map[string]bool, filled map[int]bool) bool {
	for _, want := range liveCertsEmptied {
		if line == want {
			emptied[want] = true
			return true
		}
	}
	for i, wants := range liveCertsFilled {
		for _, want := range wants {
			if line == want {
				filled[i] = true
				return true
			}
		}
	}
	return false
}

// underDest returns the part of a path in the wrapper after $DEST/.
func underDest(p string) (string, bool) {
	for _, prefix := range []string{"$DEST/", "${DEST}/"} {
		if rel, ok := strings.CutPrefix(p, prefix); ok && rel != "" && !strings.Contains(rel, "..") && !strings.ContainsAny(rel, "$`\"'") {
			return filepath.Clean(rel), true
		}
	}
	return "", false
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// normalizeExtractArgs is the arguments of a trust extract without
// --overwrite (keploy extracts into fresh paths), with "--purpose X" written
// "--purpose=X", unquoted and sorted, for comparing.
func normalizeExtractArgs(f []string) string {
	var args []string
	for i := 0; i < len(f); i++ {
		a := unquote(f[i])
		switch {
		case a == "--overwrite":
			continue
		case a == "--purpose" && i+1 < len(f):
			i++
			a = "--purpose=" + unquote(f[i])
		}
		args = append(args, a)
	}
	sort.Strings(args)
	return strings.Join(args, " ")
}

// swapFallback is why the swap left the store to the distro's own tool after
// all, having changed nothing in it: filling the staging directory failed, or
// what it holds is not something keploy knows how to put in place exactly as
// the tool would.
type swapFallback struct{ problems []string }

func (f *swapFallback) Error() string {
	return "the staged trust store is not one keploy can put in place: " + strings.Join(f.problems, "; ")
}

// swapP11KitTrust regenerates the outputs of a p11-kit trust store (Fedora,
// RHEL, Amazon Linux) from its anchors, in place and without a gap; see the
// top of this file. wrapper is the distro's update-ca-trust. It reports false,
// having changed nothing in the store, when the wrapper or its output is not
// one keploy can stand in for — and says why — and the caller runs the
// wrapper. The caller holds the trust-store lock, and ctx is the one that lock
// returned.
func swapP11KitTrust(ctx context.Context, logger *zap.Logger, wrapper string) (bool, error) {
	const gap = "regenerating the system trust store with the distro's own tool, which can leave parts of it without certificates while it runs: "
	l, err := readP11KitLayout(wrapper)
	if err != nil {
		logger.Warn(gap+"keploy could not read what that tool does", zap.String("tool", wrapper), zap.Error(err))
		return false, nil
	}
	if len(l.unknown) > 0 {
		logger.Warn(gap+"it does something keploy does not know how to do in place", zap.String("tool", wrapper), zap.Strings("unknown", l.unknown))
		return false, nil
	}
	if fi, err := os.Stat(l.dest); err != nil || !fi.IsDir() {
		logger.Warn(gap+"the store it writes is not a directory", zap.String("tool", wrapper), zap.String("store", l.dest))
		return false, nil
	}
	for _, o := range l.outputs {
		if _, err := os.Stat(o.tool); err != nil {
			logger.Warn(gap+"the extraction tool it names is not there", zap.String("tool", wrapper), zap.String("extractor", o.tool))
			return false, nil
		}
	}
	err = p11kitSwap(ctx, logger, l)
	var fb *swapFallback
	if errors.As(err, &fb) {
		logger.Warn(gap+"keploy could not stage its output, or does not know how to put what it staged in place", zap.String("tool", wrapper), zap.Strings("problems", fb.problems))
		return false, nil
	}
	return true, err
}

func p11kitSwap(ctx context.Context, logger *zap.Logger, l *p11kitLayout) error {
	clearSwapLeftovers(logger, l)

	staging, err := os.MkdirTemp(l.dest, p11kitSwapPrefix)
	if err != nil {
		return fmt.Errorf("failed to create a staging directory for the trust store: %w", err)
	}
	defer func() { _ = removeTree(staging) }()
	out := filepath.Join(staging, "out")

	// Fill the staging directory before changing anything. A failure leaves
	// the store exactly as it was and hands the job to the wrapper: it fails
	// the same way, or works where keploy's copy of its work did not, and
	// either way the store ends as the wrapper leaves it.
	if l.output {
		cmd := storeCommand(ctx, l.wrapper, "extract", "-o", out)
		if o, err := p11kitRunExtract(cmd); err != nil {
			return &swapFallback{problems: []string{fmt.Sprintf("%s extract -o failed: %v: %s", l.wrapper, err, bytes.TrimSpace(o))}}
		}
	} else {
		for _, o := range l.outputs {
			dest := filepath.Join(out, o.rel)
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return err
			}
			args := append(append([]string{"extract"}, o.args...), dest)
			cmd := storeCommand(ctx, o.tool, args...)
			// As the wrapper does: only the system's p11-kit configuration
			// counts.
			cmd.Env = append(os.Environ(), "P11_KIT_NO_USER_CONFIG=1")
			if out, err := p11kitRunExtract(cmd); err != nil {
				return &swapFallback{problems: []string{fmt.Sprintf("%s %s failed: %v: %s", o.tool, strings.Join(args, " "), err, bytes.TrimSpace(out))}}
			}
		}
		for _, link := range l.links {
			// p11-kit leaves directory-hash read-only, as the wrapper finds it.
			if err := os.Chmod(filepath.Dir(filepath.Join(out, link.rel)), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(link.target, filepath.Join(out, link.rel)); err != nil {
				return err
			}
		}
	}

	files, dirHash, err := checkStaged(l, out)
	if err != nil {
		return err
	}
	for _, rel := range files {
		if err := os.Rename(filepath.Join(out, rel), filepath.Join(l.dest, rel)); err != nil {
			return fmt.Errorf("failed to put the new %s in place: %w", rel, err)
		}
		swapStep("replaced " + rel)
	}
	if dirHash {
		if err := swapDirectoryHash(filepath.Join(l.dest, dirHashRel), filepath.Join(out, dirHashRel), l.certsDir); err != nil {
			return err
		}
	}
	logger.Debug("regenerated the system trust store in place", zap.String("store", l.dest), zap.Strings("files", files), zap.Bool("directory-hash", dirHash))
	return nil
}

// checkStaged checks that the staging directory holds only what keploy knows
// how to put in place exactly as a live run of the wrapper would leave it, and
// returns its file outputs and whether it holds directory-hash. Anything else
// is a *swapFallback:
//
//   - a directory but the store's own (openssl, pem, java, edk2) and
//     directory-hash, a symlink outside directory-hash, a file outside those
//     directories, a directory inside directory-hash;
//   - a file output whose directory the live store does not have;
//   - a known file output the live store has and the staging directory does
//     not: a live run would have left it there, or removed it (an "rm" in the
//     wrapper), and nothing tells the two apart;
//   - directory-hash staged where the live store has none;
//   - a regular file under a hash link's name in the live directory-hash or
//     the certs directory: p11-kit would replace it, and keploy replaces only
//     links.
func checkStaged(l *p11kitLayout, out string) ([]string, bool, error) {
	var files, problems []string
	dirHash := false
	err := filepath.WalkDir(out, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(out, p)
		switch {
		case rel == ".":
		case d.IsDir() && rel == dirHashRel:
			dirHash = true
			return fs.SkipDir
		case d.IsDir() && p11kitDirs[rel]:
		case d.Type().IsRegular() && p11kitDirs[filepath.Dir(rel)]:
			files = append(files, rel)
		default:
			problems = append(problems, "staged "+rel+" ("+d.Type().String()+")")
			if d.IsDir() {
				return fs.SkipDir
			}
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	staged := map[string]bool{}
	for _, rel := range files {
		staged[rel] = true
		if fi, err := os.Stat(filepath.Join(l.dest, filepath.Dir(rel))); err != nil || !fi.IsDir() {
			problems = append(problems, "the store has no directory for "+rel)
		}
	}
	for rel, o := range p11kitKnownOutputs {
		if fi, err := os.Lstat(filepath.Join(l.dest, rel)); err == nil && !o.dir && fi.Mode().IsRegular() && !staged[rel] {
			problems = append(problems, "the store has "+rel+" and the wrapper's output does not")
		}
	}
	if dirHash {
		live := filepath.Join(l.dest, dirHashRel)
		if fi, err := os.Lstat(live); err != nil || !fi.IsDir() {
			problems = append(problems, "the store has no "+dirHashRel+" to put the wrapper's in")
		}
		entries, err := os.ReadDir(filepath.Join(out, dirHashRel))
		if err != nil {
			return nil, false, err
		}
		for _, e := range entries {
			if e.IsDir() {
				problems = append(problems, "staged "+dirHashRel+"/"+e.Name()+" is a directory")
			}
		}
		for _, dir := range []string{live, l.certsDir} {
			if dir == "" {
				continue
			}
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				if hashLinkName.MatchString(e.Name()) && e.Type()&fs.ModeSymlink == 0 {
					problems = append(problems, filepath.Join(dir, e.Name())+" is not a link")
				}
			}
		}
	}
	sort.Strings(files)
	sort.Strings(problems)
	if len(problems) > 0 {
		return nil, false, &swapFallback{problems: problems}
	}
	return files, dirHash, nil
}

// swapDirectoryHash makes the live hash directory what staged is — and, when
// certsDir is set, the links in certsDir what the wrapper would make them —
// without a moment in which a certificate present in both is out of reach:
//
//  0. Every certificate file whose content is about to change is first
//     hard-linked to a hold, and the hold is linked at the end of the
//     reachable part of every chain that points at the file, here and in
//     certsDir. Two CAs with one label are X.pem and X.1.pem, and when their
//     order changes (one of them leaves, or a renewal is read first) the
//     content behind each name moves: without the hold, the CA moving from
//     X.1.pem to X.pem would be out of reach between the two renames of step 1.
//  1. Every certificate file the new chains link to goes in, renamed over
//     any old one.
//  2. The hash links next to them are rewritten (swapHashChain), which also
//     removes the links to the holds: they are past the end of every chain.
//  3. So are OpenSSL's default directory's links, as the wrapper fills it: an
//     absolute link per hash link, to the file it resolves to.
//  4. The other entries the wrapper writes — RHEL 9's and Amazon Linux's
//     compatibility links — are put in place.
//  5. Every hold comes out — this swap's, and any a swap that died left: their
//     links went in steps 2 and 3, and until then they kept what they held in
//     reach — and so does every entry the wrapper's output does not have: the
//     CAs that left, and anything else, as p11-kit's own overwrite of the
//     directory removes it.
func swapDirectoryHash(live, staged, certsDir string) error {
	// p11-kit leaves a directory it wrote read-only; its entries are moved
	// out of this one.
	if err := os.Chmod(staged, 0o700); err != nil {
		return err
	}
	base, err := filepath.EvalSymlinks(live)
	if err != nil {
		return err
	}
	want, err := readHashDir(staged)
	if err != nil {
		return err
	}
	wantNames := map[string]bool{}
	var others []string
	stagedEntries, err := os.ReadDir(staged)
	if err != nil {
		return err
	}
	for _, e := range stagedEntries {
		wantNames[e.Name()] = true
		if !want.files[e.Name()] && !(hashLinkName.MatchString(e.Name()) && e.Type()&fs.ModeSymlink != 0) {
			others = append(others, e.Name())
		}
	}
	have, err := readHashDir(live)
	if err != nil {
		return err
	}
	var inCerts hashDir
	if certsDir != "" {
		if inCerts, err = readHashDir(certsDir); err != nil {
			return err
		}
	}

	// 0. Holds.
	holds := map[string]string{}
	for _, name := range sortedKeys(want.files) {
		if !have.files[name] || sameContent(filepath.Join(live, name), filepath.Join(staged, name)) {
			continue
		}
		hold := p11kitHoldPrefix + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatUint(swapSeq.Add(1), 10)
		if err := os.Link(filepath.Join(live, name), filepath.Join(live, hold)); err != nil {
			return fmt.Errorf("failed to hold %s while it is replaced: %w", name, err)
		}
		swapStep("held " + name + " as " + hold)
		holds[name] = hold
	}
	if len(holds) > 0 {
		if err := linkHolds(live, have, func(target string) (string, bool) {
			hold, ok := holds[target]
			return hold, ok
		}); err != nil {
			return err
		}
		if certsDir != "" {
			if err := linkHolds(certsDir, inCerts, func(target string) (string, bool) {
				rel, err := filepath.Rel(base, target)
				if err != nil {
					return "", false
				}
				hold, ok := holds[rel]
				return filepath.Join(base, hold), ok
			}); err != nil {
				return err
			}
			if inCerts, err = readHashDir(certsDir); err != nil {
				return err
			}
		}
		if have, err = readHashDir(live); err != nil {
			return err
		}
	}

	// 1. Every certificate file the new chains link to, in place before any
	// link points at it.
	for _, name := range sortedKeys(want.files) {
		if sameContent(filepath.Join(live, name), filepath.Join(staged, name)) {
			continue
		}
		if err := os.Rename(filepath.Join(staged, name), filepath.Join(live, name)); err != nil {
			return fmt.Errorf("failed to put %s in place: %w", name, err)
		}
		swapStep("certificate file " + name)
	}

	// 2. The hash links next to them.
	for _, h := range chainHashes(have.chains, want.chains) {
		if err := swapHashChain(live, h, have.chains[h], chainList(want.chains[h])); err != nil {
			return err
		}
	}

	// 3. OpenSSL's default directory.
	if certsDir != "" {
		abs := make(map[string][]string, len(want.chains))
		for h, chain := range want.chains {
			for _, target := range chainList(chain) {
				abs[h] = append(abs[h], filepath.Join(base, target))
			}
		}
		for _, h := range chainHashes(inCerts.chains, abs) {
			if err := swapHashChain(certsDir, h, inCerts.chains[h], abs[h]); err != nil {
				return err
			}
		}
	}

	// 4. The wrapper's other entries.
	sort.Strings(others)
	for _, name := range others {
		src, dst := filepath.Join(staged, name), filepath.Join(live, name)
		fi, err := os.Lstat(src)
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(src)
			if err != nil {
				return err
			}
			if err := setLink(dst, target); err != nil {
				return err
			}
			continue
		}
		if sameContent(dst, src) {
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("failed to put %s in place: %w", name, err)
		}
		swapStep("put " + name + " in place")
	}

	// 5. The holds, and everything the wrapper's output does not have.
	entries, err := os.ReadDir(live)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, p11kitHoldPrefix) {
			if err := os.Remove(filepath.Join(live, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			swapStep("removed hold " + name)
		}
	}
	for _, e := range entries {
		name := e.Name()
		if wantNames[name] || strings.HasPrefix(name, p11kitSwapPrefix) || e.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(live, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		swapStep("removed " + name)
	}
	return nil
}

// linkHolds links, at the end of the reachable part of every chain in dir, the
// hold of each file a link of that chain points at; holdFor maps a link's
// target to its hold's. The reachable part ends at the first index whose link
// is missing or does not resolve — a link to a hold that a dead swap's leftover
// cleanup took away, say — because that is where OpenSSL stops.
func linkHolds(dir string, d hashDir, holdFor func(target string) (string, bool)) error {
	for _, h := range sortedKeys(d.chains) {
		chain := d.chains[h]
		end := chainReach(dir, h, chain)
		idx := make([]int, 0, len(chain))
		for i := range chain {
			if i < end {
				idx = append(idx, i)
			}
		}
		sort.Ints(idx)
		for _, i := range idx {
			hold, ok := holdFor(chain[i])
			if !ok {
				continue
			}
			if err := setHashLink(dir, h, end, hold); err != nil {
				return err
			}
			end++
		}
	}
	return nil
}

// chainReach is how far OpenSSL gets along the chain <h>.0, <h>.1, ... in dir:
// the index of the first link that is missing or does not resolve.
func chainReach(dir, h string, chain map[int]string) int {
	n := 0
	for {
		if _, ok := chain[n]; !ok {
			return n
		}
		if _, err := os.Stat(filepath.Join(dir, h+"."+strconv.Itoa(n))); err != nil {
			return n
		}
		n++
	}
}

// swapHashChain rewrites the chain of links named <h>.0, <h>.1, ... in dir from
// have (index to target) to want, and never leaves a certificate of want out of
// reach meanwhile.
//
// OpenSSL looks a CA up in a hashed directory by trying <h>.0, <h>.1, ... and
// stops at the first name that does not exist, so a chain is only as good as
// its unbroken prefix. Replacing <h>.0 first could hide a certificate that the
// new chain keeps but at another index, and a link removed from the middle of
// a chain hides every one after it. So, when an existing link has to change:
//
//	A. the whole new chain is first linked after the reachable end of the old
//	   one (where nothing reachable is replaced);
//	B. then each index is pointed at its new target, in order — whatever it
//	   replaces at index j is already at its own final index, or still in the
//	   copy from A, which stays reachable from index 0;
//	C. then every link past the new chain's end is removed: the new chain is
//	   whole at the front by then, so nothing past it is needed.
//
// When no existing link has to change — a CA added or removed alone, by far
// the usual case — A is skipped: B then only adds names past the old end, and
// C only removes past the new one.
func swapHashChain(dir, h string, have map[int]string, want []string) error {
	// Reachable is what OpenSSL gets to: a link that does not resolve ends
	// the chain as surely as a missing one.
	reach := chainReach(dir, h, have)
	if reach == len(want) && len(have) == len(want) {
		same := true
		for j, t := range want {
			if have[j] != t {
				same = false
				break
			}
		}
		if same {
			return nil
		}
	}
	replaces := false
	for j := 0; j < reach && j < len(want); j++ {
		if have[j] != want[j] {
			replaces = true
			break
		}
	}
	if replaces {
		for j, t := range want {
			if err := setHashLink(dir, h, reach+j, t); err != nil {
				return err
			}
		}
	}
	for j, t := range want {
		if err := setHashLink(dir, h, j, t); err != nil {
			return err
		}
	}
	top := len(want) - 1
	if replaces {
		top = reach + len(want) - 1
	}
	for i := range have {
		if i > top {
			top = i
		}
	}
	for i := len(want); i <= top; i++ {
		if err := removeHashLink(dir, h, i); err != nil {
			return err
		}
	}
	return nil
}

var swapSeq atomic.Uint64

// setHashLink makes dir/<h>.<i> a symlink to target (setLink).
func setHashLink(dir, h string, i int, target string) error {
	return setLink(filepath.Join(dir, h+"."+strconv.Itoa(i)), target)
}

// setLink makes name a symlink to target: created if absent, else replaced by
// renaming a new link over it, so that at every instant name is the old link
// or the new one. It never replaces anything that is not a symlink — no tool
// keploy mimics writes a regular file there, so one is somebody else's.
func setLink(name, target string) error {
	dir := filepath.Dir(name)
	fi, err := os.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Symlink(target, name); err != nil {
			return err
		}
	case err != nil:
		return err
	case fi.Mode()&fs.ModeSymlink == 0:
		return fmt.Errorf("%s is not a symlink; refusing to replace it", name)
	default:
		if cur, err := os.Readlink(name); err == nil && cur == target {
			return nil
		}
		tmp := filepath.Join(dir, p11kitSwapPrefix+strconv.Itoa(os.Getpid())+"-"+strconv.FormatUint(swapSeq.Add(1), 10))
		if err := os.Symlink(target, tmp); err != nil {
			return err
		}
		if err := os.Rename(tmp, name); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	swapStep("link " + name + " -> " + target)
	return nil
}

// removeHashLink removes dir/<h>.<i> if it is a symlink.
func removeHashLink(dir, h string, i int) error {
	name := filepath.Join(dir, h+"."+strconv.Itoa(i))
	fi, err := os.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) || err == nil && fi.Mode()&fs.ModeSymlink == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.Remove(name); err != nil {
		return err
	}
	swapStep("removed link " + name)
	return nil
}

var hashLinkName = regexp.MustCompile(`^([0-9a-f]{8})\.([0-9]+)$`)

// hashDir is what of a hash directory the swap manages: the certificate files
// (*.pem) and the hash links, by hash and index. Everything else — RHEL 9's two
// compatibility links in directory-hash, an anchor keploy wrote into
// /etc/ssl/certs on RHEL, an administrator's own file — is left alone.
type hashDir struct {
	files  map[string]bool
	chains map[string]map[int]string
}

func readHashDir(dir string) (hashDir, error) {
	d := hashDir{files: map[string]bool{}, chains: map[string]map[int]string{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return d, err
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, p11kitSwapPrefix) {
			continue
		}
		if m := hashLinkName.FindStringSubmatch(name); m != nil && e.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(filepath.Join(dir, name))
			if err != nil {
				return d, err
			}
			i, err := strconv.Atoi(m[2])
			if err != nil {
				continue
			}
			if d.chains[m[1]] == nil {
				d.chains[m[1]] = map[int]string{}
			}
			d.chains[m[1]][i] = target
			continue
		}
		if e.Type().IsRegular() && strings.HasSuffix(name, ".pem") {
			d.files[name] = true
		}
	}
	return d, nil
}

// chainHashes returns every hash either side has, in order.
func chainHashes[A, B any](have map[string]A, want map[string]B) []string {
	set := map[string]bool{}
	for h := range have {
		set[h] = true
	}
	for h := range want {
		set[h] = true
	}
	return sortedKeys(set)
}

// chainList is a chain p11-kit wrote, as a list. Such a chain has no gaps, so
// its order is its indices'.
func chainList(m map[int]string) []string {
	idx := make([]int, 0, len(m))
	for i := range m {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	out := make([]string, 0, len(idx))
	for _, i := range idx {
		out = append(out, m[i])
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sameContent reports whether two regular files hold the same bytes.
func sameContent(a, b string) bool {
	fa, err := os.Lstat(a)
	if err != nil || !fa.Mode().IsRegular() {
		return false
	}
	da, err := os.ReadFile(a)
	if err != nil {
		return false
	}
	db, err := os.ReadFile(b)
	return err == nil && bytes.Equal(da, db)
}

// clearSwapLeftovers removes what a swap that died left: its staging directory
// and any replacement link it had not renamed into place yet. Its holds stay:
// a hold can be the only thing that keeps a CA in reach — the swap died after
// it renamed new content over the file the hold keeps — so it goes only when
// the next swap has rewritten the chains that link to it (swapDirectoryHash,
// step 5). The caller holds the trust-store lock, so no swap is running.
func clearSwapLeftovers(logger *zap.Logger, l *p11kitLayout) {
	dirs := []string{l.dest, filepath.Join(l.dest, dirHashRel)}
	if l.certsDir != "" {
		dirs = append(dirs, l.certsDir)
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), p11kitSwapPrefix) || strings.HasPrefix(e.Name(), p11kitHoldPrefix) {
				continue
			}
			p := filepath.Join(dir, e.Name())
			if err := removeTree(p); err != nil {
				logger.Debug("could not remove what an earlier trust-store swap left", zap.String("path", p), zap.Error(err))
			}
		}
	}
}

// removeTree is os.RemoveAll for a tree p11-kit made read-only.
func removeTree(path string) error {
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(path)
}

func swapStep(change string) {
	if p11kitSwapStep != nil {
		p11kitSwapStep(change)
	}
}

// systemTrustGenerates reports whether path is, after symlinks, one of the
// outputs the store's regeneration writes: the distro's update-ca-trust (when
// it is the tool this machine regenerates with) extracts it, whether keploy
// swaps it in or runs the tool. The JDK's cacerts on Fedora, RHEL and Amazon
// Linux is java/cacerts. keploy must not write such a file itself: keytool
// rewrites a keystore in place (truncate, then write), which leaves every JVM
// that starts meanwhile with an empty or torn truststore, and the regeneration
// replaces the file anyway — with this run's CA in it, from its anchor.
func systemTrustGenerates(path string) bool {
	wrapper := storeTool()
	if filepath.Base(wrapper) != "update-ca-trust" {
		return false
	}
	l, err := readP11KitLayout(wrapper)
	if err != nil {
		return false
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	for _, rel := range l.generated {
		if out, err := filepath.EvalSymlinks(filepath.Join(l.dest, rel)); err == nil && out == real {
			return true
		}
	}
	return false
}
