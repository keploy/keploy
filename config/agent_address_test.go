package config

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Until a Setup sets the agent's address, it is the one the caller set in
// Agent.AgentURI; once one has, it is the one set last.
func TestTheAgentAddressIsTheCallersUntilSetThenTheLastSet(t *testing.T) {
	c := &Config{}
	c.Agent.AgentURI = "http://10.0.0.7:8096/agent"
	if got := c.AgentURI(); got != "http://10.0.0.7:8096/agent" {
		t.Fatalf("AgentURI() = %q before any SetAgentURI; want the caller's Agent.AgentURI", got)
	}
	c.SetAgentURI("http://localhost:40001/agent")
	c.SetAgentURI("http://localhost:40002/agent")
	if got := c.AgentURI(); got != "http://localhost:40002/agent" {
		t.Fatalf("AgentURI() = %q; want the address set last, http://localhost:40002/agent", got)
	}
}

// A copy of a Config is a Config of its own: a move of the copy's agent (the
// replay stack's, set up from a copy) is not a move of the original's, and
// the reverse.
func TestACopysAgentAddressIsItsOwn(t *testing.T) {
	orig := &Config{}
	orig.Agent.AgentURI = "http://localhost:40001/agent"
	orig.SetAgentURI("http://localhost:40001/agent")
	cp := *orig
	if got := cp.AgentURI(); got != "http://localhost:40001/agent" {
		t.Fatalf("a copy's AgentURI() = %q; want the address its Agent.AgentURI was copied with", got)
	}
	cp.SetAgentURI("http://localhost:40009/agent")
	if got := orig.AgentURI(); got != "http://localhost:40001/agent" {
		t.Fatalf("the original's AgentURI() = %q after its copy's agent moved; want http://localhost:40001/agent", got)
	}
	orig.SetAgentURI("http://localhost:40002/agent")
	if got := cp.AgentURI(); got != "http://localhost:40009/agent" {
		t.Fatalf("the copy's AgentURI() = %q after the original's agent moved; want http://localhost:40009/agent", got)
	}
}

// The address moves while it is read (run with -race): each read is one of
// the addresses set, whole, and the last one set is read once the moves end.
func TestTheAgentAddressMovesSafelyWhileRead(t *testing.T) {
	c := &Config{}
	c.SetAgentURI("http://localhost:40000/agent")
	const moves = 200
	set := map[string]bool{"http://localhost:40000/agent": true}
	for i := 1; i <= moves; i++ {
		set[fmt.Sprintf("http://localhost:%d/agent", 40000+i)] = true
	}
	stop := make(chan struct{})
	var readers sync.WaitGroup
	bad := make(chan string, 8)
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if got := c.AgentURI(); !set[got] {
					select {
					case bad <- got:
					default:
					}
					return
				}
			}
		}()
	}
	for i := 1; i <= moves; i++ {
		c.SetAgentURI(fmt.Sprintf("http://localhost:%d/agent", 40000+i))
	}
	close(stop)
	readers.Wait()
	close(bad)
	for got := range bad {
		t.Errorf("a reader read %q while the address moved; want one of the addresses set", got)
	}
	if got, want := c.AgentURI(), fmt.Sprintf("http://localhost:%d/agent", 40000+moves); got != want {
		t.Fatalf("AgentURI() = %q once the moves ended; want %q", got, want)
	}
}

// Every reader of the agent's address reads it through Config.AgentURI: a
// read of the Agent.AgentURI field (or a pointer to it kept) does not see the
// compose start's move of the agent, and races with it. Writes (a caller's
// preset, Setup's record of what it chose) are not reads. Test files are not
// scanned: they set the address up.
func TestNoReaderReadsTheAgentAddressField(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("the module's root is not %s: %v", root, err)
	}
	var reads []string
	scanned := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if rel == filepath.Join("config", "agent_address.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", rel, perr)
		}
		scanned++
		for _, pos := range agentAddressFieldReads(f) {
			reads = append(reads, fmt.Sprintf("%s:%d", rel, fset.Position(pos).Line))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 100 {
		t.Fatalf("scanned %d files under %s; the scan did not walk the module", scanned, root)
	}
	if len(reads) > 0 {
		t.Fatalf("the agent's address is read from the Agent.AgentURI field, not through Config.AgentURI(), at:\n  %s", strings.Join(reads, "\n  "))
	}
}

// agentAddressFieldReads is where f reads X.Agent.AgentURI (or
// X.Agent.SetupOptions.AgentURI): every use of it that is not the left side
// of a plain assignment.
func agentAddressFieldReads(f *ast.File) []token.Pos {
	written := map[ast.Expr]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if as, ok := n.(*ast.AssignStmt); ok && as.Tok == token.ASSIGN {
			for _, l := range as.Lhs {
				written[l] = true
			}
		}
		return true
	})
	var reads []token.Pos
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "AgentURI" || written[sel] {
			return true
		}
		x, ok := sel.X.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if x.Sel.Name == "SetupOptions" {
			if xx, ok := x.X.(*ast.SelectorExpr); ok {
				x = xx
			}
		}
		if x.Sel.Name == "Agent" {
			reads = append(reads, sel.Pos())
		}
		return true
	})
	return reads
}

// The scan finds the reads it is there to find, and passes the writes.
func TestTheAgentAddressScanFindsReadsNotWrites(t *testing.T) {
	src := `package p
func f(c *C, p *string) string {
	c.Agent.AgentURI = "w"
	c.Agent.SetupOptions.AgentURI = "w"
	_ = c.Agent.AgentURI
	p = &c.Agent.AgentURI
	if c.Agent.SetupOptions.AgentURI != "" {}
	g(c.Agent.AgentURI)
	_ = opts.AgentURI
	return c.AgentURI()
}`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var lines []int
	for _, pos := range agentAddressFieldReads(f) {
		lines = append(lines, fset.Position(pos).Line)
	}
	if got, want := fmt.Sprint(lines), "[5 6 7 8]"; got != want {
		t.Fatalf("reads found on lines %s; want %s", got, want)
	}
}
