package kubeconfig

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Written is what WriteBack did with the user's kubeconfig file.
type Written struct {
	// Changes is what the test changed in its copy (Diff).
	Changes
	// File is the file written: the user's kubeconfig, its symlinks followed.
	File string
	// Backup is the user's file as it was before keploy wrote it, kept beside
	// it; "" when keploy wrote nothing.
	Backup string
	// Conflict is each entry the test changed that was also changed in the
	// user's file during the run, by someone other than the test, as
	// "cluster NAME", "context NAME", "user NAME", "current-context" or
	// "preferences". keploy then writes nothing to the user's file.
	Conflict []string
	// Kept is, when keploy wrote nothing although the test changed its copy,
	// a kubeconfig beside the user's holding what the test added and changed
	// (as the test left it, without keploy's CA), for the user to merge.
	Kept string
}

// WriteOptions are what WriteBack takes from keploy's run.
type WriteOptions struct {
	// Now names the files keploy keeps beside the user's (Backup, Kept).
	Now time.Time
	// Chown gives path, a file keploy wrote, to the owner of ref, the
	// user's file (keploy under sudo); nil leaves it keploy's.
	Chown func(ref, path string) error
}

// WriteBack writes to file, the user's kubeconfig, what the test changed in
// keploy's copy of it at copyPath: orig is the user's file as keploy read it
// as the run started, wrote the copy as keploy wrote it from orig (Rewrite),
// now the copy as the test left it, and ca keploy's CA certificate (PEM).
//
// Only what the test changed is written: each cluster, context and user it
// added, changed or removed, its current context and its preferences. Of an
// entry it changed, only the fields it changed: what keploy itself set in the
// copy (its CA beside a cluster's own, a path made absolute) never reaches the
// user's file, and keploy's CA is taken out of a CA the test wrote. With
// KUBECONFIG listing several files, keploy made a copy of each, in order, and
// kubectl wrote each entry to the copy of the file it would have written, so
// each copy's changes go to its own file.
//
// The user's file is edited in place: its comments and the order of its
// entries are kept. It is first kept, as it was, in a backup beside it
// (FILE.keploy-backup-STAMP, its owner's and mode), then replaced at once by
// a new file renamed over it. Nothing is written, and no backup is made, when
// the test changed nothing. When the user's file changed during the run, the
// test's changes are merged onto it as it is now, unless an entry the test
// changed was changed in it too: then nothing is written, the conflicting
// entries are named (Conflict), and the test's changes are kept beside the
// user's file (Kept).
func WriteBack(file, copyPath string, orig, wrote, now, ca []byte, opt WriteOptions) (Written, error) {
	ch, err := Diff(copyPath, wrote, now, ca)
	if err != nil {
		return Written{}, err
	}
	w := Written{Changes: ch}
	if ch.Empty() {
		return w, nil
	}
	eds := editsOf(ch, rootOrNil(wrote), rootOrNil(now))
	target, err := filepath.EvalSymlinks(file)
	if err != nil {
		return w, w.keep(file, eds, ca, opt, fmt.Errorf("keploy could not find the kubeconfig %s: %w", file, err))
	}
	w.File = target
	info, err := os.Stat(target)
	if err != nil {
		return w, w.keep(target, eds, ca, opt, err)
	}
	cur, err := os.ReadFile(target)
	if err != nil {
		return w, w.keep(target, eds, ca, opt, fmt.Errorf("keploy could not read the kubeconfig %s: %w", target, err))
	}
	if !bytes.Equal(cur, orig) {
		w.Conflict, err = conflictsOf(eds, orig, cur)
		if err != nil {
			return w, w.keep(target, eds, ca, opt, fmt.Errorf("the kubeconfig %s, as it is now: %w", target, err))
		}
		if len(w.Conflict) > 0 {
			return w, w.keep(target, eds, ca, opt, nil)
		}
	}
	out, err := applied(cur, eds, ca)
	if err != nil {
		return w, w.keep(target, eds, ca, opt, fmt.Errorf("the kubeconfig %s, as it is now: %w", target, err))
	}
	mode := info.Mode().Perm()
	backup, err := writeNew(target, ".keploy-backup-", cur, mode, opt)
	if err != nil {
		return w, w.keep(target, eds, ca, opt, fmt.Errorf("keploy could not back up the kubeconfig %s: %w", target, err))
	}
	if err := replace(target, out, mode, opt); err != nil {
		// The backup is the file as it still is: it is not kept.
		_ = os.Remove(backup)
		return w, w.keep(target, eds, ca, opt, fmt.Errorf("keploy could not write the kubeconfig %s: %w", target, err))
	}
	w.Backup = backup
	return w, nil
}

// keep writes, when keploy writes nothing to the user's file, what the test
// added and changed to a kubeconfig beside file (Kept), and returns err. A
// change that only removes leaves nothing to keep: Changes names it.
func (w *Written) keep(file string, eds []edit, ca []byte, opt WriteOptions, err error) error {
	doc := newConfig()
	root := doc.Content[0]
	for _, e := range eds {
		if e.to == nil {
			continue
		}
		if e.list == "" {
			setNode(root, e.key, clone(e.to))
			continue
		}
		appendItem(root, e.list, e.item(ca))
	}
	if len(root.Content) == 4 { // apiVersion and kind alone
		return err
	}
	data, merr := encode(doc)
	if merr == nil {
		mode := os.FileMode(0o600) // what the test wrote may hold keys
		w.Kept, merr = writeNew(file, ".keploy-test-changes-", data, mode, opt)
	}
	if merr != nil {
		return errors.Join(err, fmt.Errorf("keploy could not keep the test's changes beside it: %w", merr))
	}
	return err
}

// edit is one entry the test added, changed or removed: a named item of a
// list (clusters, contexts, users), or a key of the config itself
// (current-context, preferences). from is it in the copy keploy wrote, to as
// the test left it; nil where it is not.
type edit struct {
	what              string // as Changes and Conflict name it
	list, field, name string // an item: its list, its field and name
	key               string // else: the config's key
	from, to          *yaml.Node
}

// item is to, the item as the test left it, with keploy's CA taken out of a
// cluster's CA.
func (e edit) item(ca []byte) *yaml.Node {
	it := clone(e.to)
	if e.list == "clusters" {
		if d := get(get(it, e.field), "certificate-authority-data"); d.Value != "" {
			if v := withoutCA(d.Value, ca); v != "" {
				setString(d, v)
			} else {
				del(get(it, e.field), "certificate-authority-data")
			}
		}
	}
	return it
}

var lists = map[string]struct{ list, field string }{
	"cluster": {"clusters", "cluster"},
	"context": {"contexts", "context"},
	"user":    {"users", "user"},
}

// editsOf is each change ch names, from the copy keploy wrote (wrote) to the
// one the test left (now): clusters, contexts and users as Diff orders them,
// then the current context and the preferences.
func editsOf(ch Changes, wrote, now *yaml.Node) []edit {
	var eds []edit
	for _, names := range [][]string{ch.Added, ch.Changed, ch.Removed} {
		for _, what := range names {
			kind, name, _ := strings.Cut(what, " ")
			l := lists[kind]
			eds = append(eds, edit{what: what, list: l.list, field: l.field, name: name,
				from: itemNamed(wrote, l.list, name), to: itemNamed(now, l.list, name)})
		}
	}
	if ch.CurrentContext != "" {
		eds = append(eds, edit{what: "current-context", key: "current-context", from: valueOf(wrote, "current-context"), to: valueOf(now, "current-context")})
	}
	if ch.Preferences {
		eds = append(eds, edit{what: "preferences", key: "preferences", from: valueOf(wrote, "preferences"), to: valueOf(now, "preferences")})
	}
	return eds
}

// conflictsOf is each of eds whose entry was changed in the user's file
// during the run (it differs between orig, the file as the run started, and
// cur, the file now), unless to what the test changed it to.
func conflictsOf(eds []edit, orig, cur []byte) ([]string, error) {
	was, err := rootOf(orig)
	if err != nil {
		return nil, err
	}
	is, err := rootOf(cur)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range eds {
		a, b := valueOf(was, e.key), valueOf(is, e.key)
		if e.list != "" {
			a, b = itemNamed(was, e.list, e.name), itemNamed(is, e.list, e.name)
		}
		if !same(a, b) && !same(b, e.to) {
			out = append(out, e.what)
		}
	}
	return out, nil
}

// applied is cur, the user's file, with eds made in it.
func applied(cur []byte, eds []edit, ca []byte) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(cur, &doc); err != nil {
		return nil, fmt.Errorf("not YAML kubectl reads: %w", err)
	}
	if len(doc.Content) == 0 {
		doc = *newConfig()
	}
	root := deref(doc.Content[0])
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("not a YAML mapping")
	}
	for _, e := range eds {
		switch {
		case e.list == "" && e.to == nil:
			del(root, e.key)
		case e.list == "":
			setNode(root, e.key, clone(e.to))
		case e.to == nil:
			removeItem(root, e.list, e.name)
		case e.from == nil:
			appendItem(root, e.list, e.item(ca))
		default:
			mine := itemNamed(root, e.list, e.name)
			if mine == nil {
				appendItem(root, e.list, e.item(ca))
				continue
			}
			body, ok := lookup(mine, e.field)
			if !ok || body.Kind != yaml.MappingNode {
				setNode(mine, e.field, clone(get(e.item(ca), e.field)))
				continue
			}
			merge(body, get(e.from, e.field), get(e.to, e.field), e.list == "clusters", ca)
		}
	}
	return encode(&doc)
}

const (
	fieldCAData = "certificate-authority-data"
	fieldCAFile = "certificate-authority"
)

// merge makes in mine, the user's mapping, what changed from from, keploy's
// copy, to to, the copy as the test left it, field by field: a field the test
// left as keploy wrote it stays as the user wrote it, so what keploy itself
// set never reaches the user's file. A mapping both hold is merged the same
// way. Of a cluster (isCluster), the CA is one choice: when the test wrote
// its CA data, the user's CA becomes what the test wrote, without keploy's
// CA, and its CA file the test's too (kubectl reads the data before the file,
// and keploy had moved the file's CA into the data).
func merge(mine, from, to *yaml.Node, isCluster bool, ca []byte) {
	skip := map[string]bool{}
	if isCluster {
		skip[fieldCAData], skip[fieldCAFile] = true, true
		if !same(field(from, fieldCAData), field(to, fieldCAData)) {
			if d := field(to, fieldCAData); d != nil && withoutCA(d.Value, ca) != "" {
				set(mine, fieldCAData, withoutCA(d.Value, ca))
			} else {
				del(mine, fieldCAData)
			}
			setOrDel(mine, fieldCAFile, field(to, fieldCAFile))
		} else if !same(field(from, fieldCAFile), field(to, fieldCAFile)) {
			setOrDel(mine, fieldCAFile, field(to, fieldCAFile))
		}
	}
	for _, k := range keysOf(to, from) {
		a, b := field(from, k), field(to, k)
		if skip[k] || same(a, b) {
			continue
		}
		if m := field(mine, k); m != nil && m.Kind == yaml.MappingNode && a != nil && a.Kind == yaml.MappingNode && b != nil && b.Kind == yaml.MappingNode {
			merge(m, a, b, false, ca)
			continue
		}
		setOrDel(mine, k, b)
	}
}

// withoutCA is v, base64 PEM, with keploy's CA (ca) taken out, and "" when
// nothing else is left. A value that is no base64 is kept as it is.
func withoutCA(v string, ca []byte) string {
	own, err := base64.StdEncoding.DecodeString(v)
	ca = bytes.TrimSpace(ca)
	if err != nil || len(ca) == 0 || !bytes.Contains(own, ca) {
		return v
	}
	rest := bytes.TrimSpace(bytes.ReplaceAll(own, ca, nil))
	if len(rest) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(append(rest, '\n'))
}

// same is whether a and b, either nil when absent, hold the same once what
// holds nothing is left out (pruned): how kubectl writes it aside.
func same(a, b *yaml.Node) bool {
	return reflect.DeepEqual(prunedOf(a), prunedOf(b))
}

func prunedOf(n *yaml.Node) any {
	if n == nil {
		return nil
	}
	var v any
	if err := n.Decode(&v); err != nil {
		return n.Value
	}
	if v = pruned(v); empty(v) {
		return nil
	}
	return v
}

// rootOf is the config data holds, or nil when it holds none (an empty
// file). A root that is no mapping, null too, is refused, as Rewrite and
// applied refuse it.
func rootOf(data []byte) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("not YAML kubectl reads: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	root := deref(doc.Content[0])
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("not a YAML mapping")
	}
	return root, nil
}

func rootOrNil(data []byte) *yaml.Node {
	root, _ := rootOf(data)
	return root
}

// itemNamed is the first item of root's list that is a mapping named name
// (kubectl reads the first of a name), or nil.
func itemNamed(root *yaml.Node, list, name string) *yaml.Node {
	if root == nil {
		return nil
	}
	for _, it := range get(root, list).Content {
		if isNamed(it, name) {
			return deref(it)
		}
	}
	return nil
}

// isNamed is whether it, an item of a list, is a mapping named name.
func isNamed(it *yaml.Node, name string) bool {
	it = deref(it)
	return it.Kind == yaml.MappingNode && get(it, "name").Value == name
}

// valueOf is the value of key in mapping m, or nil.
func valueOf(m *yaml.Node, key string) *yaml.Node {
	if m == nil {
		return nil
	}
	n, _ := lookup(m, key)
	return n
}

func field(m *yaml.Node, key string) *yaml.Node { return valueOf(m, key) }

// keysOf is the keys of each of ms, each once, in order.
func keysOf(ms ...*yaml.Node) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range ms {
		for i := 0; i+1 < len(m.Content); i += 2 {
			if k := m.Content[i].Value; !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	return out
}

// setNode gives key in mapping m the value v, adding the key when absent.
func setNode(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = v
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
}

// setOrDel gives key in m a copy of v, or removes it when v is nil.
func setOrDel(m *yaml.Node, key string, v *yaml.Node) {
	if v == nil {
		del(m, key)
		return
	}
	setNode(m, key, clone(v))
}

// appendItem adds item to root's list, replacing the first item of its name.
func appendItem(root *yaml.Node, list string, item *yaml.Node) {
	l, ok := lookup(root, list)
	if !ok {
		l = &yaml.Node{}
		setNode(root, list, l)
	}
	if l.Kind != yaml.SequenceNode {
		// null, or what kubectl would not read: a list now.
		*l = yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	}
	l.Style = 0 // a list written [] goes on as a block
	name := get(item, "name").Value
	for i, it := range l.Content {
		if isNamed(it, name) {
			l.Content[i] = item
			return
		}
	}
	l.Content = append(l.Content, item)
}

// removeItem removes each item of root's list named name; a list root does
// not hold is left so.
func removeItem(root *yaml.Node, list, name string) {
	l := get(root, list)
	kept := l.Content[:0]
	for _, it := range l.Content {
		if !isNamed(it, name) {
			kept = append(kept, it)
		}
	}
	l.Content = kept
}

// clone is a deep copy of n, with each alias replaced by what it stands for.
func clone(n *yaml.Node) *yaml.Node {
	n = deref(n)
	c := *n
	c.Content = make([]*yaml.Node, len(n.Content))
	for i, e := range n.Content {
		c.Content[i] = clone(e)
	}
	c.Anchor = ""
	return &c
}

func newConfig() *yaml.Node {
	root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	set(root, "apiVersion", "v1")
	set(root, "kind", "Config")
	return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
}

// encode writes doc as kubectl does, two spaces deep.
func encode(doc *yaml.Node) ([]byte, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// writeNew writes data to a new file beside ref named ref+infix+STAMP (a
// number after it when that is taken), with mode, its owner ref's, and
// returns its path.
func writeNew(ref, infix string, data []byte, mode os.FileMode, opt WriteOptions) (string, error) {
	base := ref + infix + opt.Now.UTC().Format("20060102T150405Z")
	for i := 0; ; i++ {
		p := base
		if i > 0 {
			p = fmt.Sprintf("%s-%d", base, i)
		}
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if errors.Is(err, os.ErrExist) && i < 100 {
			continue
		}
		if err != nil {
			return "", err
		}
		if err := finish(f, ref, data, mode, opt); err != nil {
			return "", err // finish removed it
		}
		return p, nil
	}
}

// replace makes the file at path hold data, at once: a temporary file in its
// directory, with mode and path's owner, renamed over it.
func replace(path string, data []byte, mode os.FileMode, opt WriteOptions) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".keploy-*")
	if err != nil {
		return err
	}
	if err := finish(f, path, data, mode, opt); err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	return nil
}

// finish writes data to f, sets its mode (keploy's umask aside) and owner
// (ref's), and closes it. As kubectl writes a kubeconfig, it does not wait
// for the disk (fsync): a busy disk would hold the run's end for seconds.
func finish(f *os.File, ref string, data []byte, mode os.FileMode, opt WriteOptions) error {
	_, err := f.Write(data)
	if err == nil {
		err = f.Chmod(mode)
	}
	if err == nil && opt.Chown != nil {
		err = opt.Chown(ref, f.Name())
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}
