package containerkey

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"go.keploy.io/server/v3/pkg/matcher/learn"
)

// appContainer is a running container in the keploy agent's PID namespace
// (an app container: docker run's --pid=container:<agent>, compose's pid:
// service:<agent>), of environment env, at ip.
func appContainer(ip string, env ...string) container.InspectResponse {
	c := running("app:1", "sha256:a11", []string{ip}, nil)
	c.Config.Env = env
	c.HostConfig = &container.HostConfig{PidMode: "container:keploy-v3"}
	return c
}

// TestTheCLIHandsEachAppContainersZoneToItsAgent: the CLI reads the zone of
// each container in another's PID namespace (compose's services, each its
// own zone: TZ first, else its /etc/localtime, linked or copied) with its
// configuration, and its list hands each one to the docker-mode agent, over
// the wire, as that container's: the agent reads the same zone from it
// (learn.ZoneOfFile). A container whose zone keploy cannot tell (a JVM's
// option) is handed as unknown, with why; one in a PID namespace of its own
// (a database) or the host's has no zone read at all, and the agent holds
// none for it.
func TestTheCLIHandsEachAppContainersZoneToItsAgent(t *testing.T) {
	kolkata, berlin := zoneData(t, "Asia/Kolkata"), zoneData(t, "Europe/Berlin")
	zi := "/usr/share/zoneinfo/"
	f := newFakeDocker()
	f.set("web", appContainer("172.17.0.4", "TZ=Asia/Kolkata"))
	f.set("worker", appContainer("172.17.0.5"))
	f.set("copied", appContainer("172.17.0.6"))
	f.set("plain", appContainer("172.17.0.7"))
	f.set("jvm", appContainer("172.17.0.8", "JAVA_TOOL_OPTIONS=-Duser.timezone=Asia/Tokyo"))
	db := running("postgres:16", "sha256:feed", []string{"172.17.0.2"}, nil)
	db.HostConfig = &container.HostConfig{PidMode: ""}
	f.set("db", db)
	hostPID := running("agent:1", "sha256:a9e", []string{"172.17.0.3"}, nil)
	hostPID.HostConfig = &container.HostConfig{PidMode: "host"}
	f.set("hostpid", hostPID)
	f.zoneFiles = map[string]*fakeZoneAPI{
		"web":     {t: t, files: map[string]zoneFile{"/etc/localtime": {link: zi + "Europe/Berlin"}, zi + "Europe/Berlin": {data: berlin}, zi + "Asia/Kolkata": {data: kolkata}}},
		"worker":  {t: t, files: map[string]zoneFile{"/etc/localtime": {link: zi + "Europe/Berlin"}, zi + "Europe/Berlin": {data: berlin}}},
		"copied":  {t: t, files: map[string]zoneFile{"/etc/localtime": {data: berlin}, "/etc/timezone": {data: []byte("Europe/Berlin\n")}, zi + "Europe/Berlin": {data: berlin}}},
		"plain":   {t: t, files: map[string]zoneFile{"/etc/hostname": {data: []byte("plain\n")}}},
		"db":      {t: t, files: map[string]zoneFile{"/etc/localtime": {link: zi + "Asia/Kolkata"}, zi + "Asia/Kolkata": {data: kolkata}}},
		"hostpid": {t: t, files: map[string]zoneFile{"/etc/localtime": {link: zi + "Asia/Kolkata"}, zi + "Asia/Kolkata": {data: kolkata}}},
	}
	r, _ := watching(t, f, "127.0.0.1")
	s, _, ok := r.snapshot("")
	if !ok {
		t.Fatal("the CLI has no list to give")
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var wire Snapshot
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	p := newPushedIndex()
	p.apply(wire)
	july := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	for _, r := range []struct {
		id, want string
		offset   int
	}{
		{"web", "Asia/Kolkata", 19800},
		{"worker", "Europe/Berlin", 7200},
		{"copied", "Europe/Berlin", 7200},
		{"plain", "UTC", 0},
	} {
		z, ok := p.ContainerZone(t.Context(), r.id)
		if !ok || z.Err != "" {
			t.Fatalf("the agent holds no zone of %s (%+v, %v)", r.id, z, ok)
		}
		got, err := learn.ZoneOfFile(z.Name, z.Data)
		if err != nil {
			t.Fatalf("the agent cannot read %s's zone: %v", r.id, err)
		}
		if got.Loc.String() != r.want {
			t.Fatalf("the agent read %s's zone as %q, want %q", r.id, got.Loc, r.want)
		}
		if _, off := july.In(got.Loc).Zone(); off != r.offset {
			t.Fatalf("%s's zone is %d s east of UTC in July, want %d", r.id, off, r.offset)
		}
	}
	if z, ok := p.ContainerZone(t.Context(), "jvm"); !ok || z.Err == "" || z.Data != nil {
		t.Fatalf("a container whose zone keploy cannot tell is handed as %+v, %v; want unknown, with why", z, ok)
	}
	// Docker always reports a HostConfig: a database's PidMode is empty, a
	// --pid=host container's "host". Neither shares the agent's PID
	// namespace, so neither is read.
	for _, id := range []string{"db", "hostpid"} {
		if z, ok := p.ContainerZone(t.Context(), id); ok {
			t.Fatalf("the agent holds zone %+v of %s, a container in a PID namespace not the agent's", z, id)
		}
		f.mu.Lock()
		reads := f.zoneReads[id]
		f.mu.Unlock()
		if reads != 0 {
			t.Fatalf("the CLI read %d files of %s, a container in a PID namespace not the agent's; it has no zone to hand", reads, id)
		}
	}
}

// TestAPushedAgentWaitsForAnAppContainersZone: an app container the CLI is
// still reading has its zone waited for, event-driven, until a list carries
// it (no clock bounds the wait); a container the CLI read with no zone, or
// whose read stalled, or that is gone, is unknown at once.
func TestAPushedAgentWaitsForAnAppContainersZone(t *testing.T) {
	p := newPushedIndex()
	p.apply(Snapshot{Source: "s", Seq: 1, Containers: []SnapshotContainer{
		{ID: "late", Image: "app:1", IPs: []string{"172.17.0.4"}, Reading: true},
		{ID: "stuck", Image: "app:1", IPs: []string{"172.17.0.5"}, Reading: true, Stalled: true},
		{ID: "none", Image: "app:1", IPs: []string{"172.17.0.6"}, Fingerprint: fpDefault},
	}})
	for _, id := range []string{"none", "stuck", "gone"} {
		if z, ok := p.ContainerZone(t.Context(), id); ok {
			t.Fatalf("%s's zone = %+v; want none, at once", id, z)
		}
	}
	waits := make(chan string, 1)
	p.zoneWaits = func(id string) {
		select {
		case waits <- id:
		default:
		}
	}
	got := make(chan Zone, 1)
	go func() {
		z, _ := p.ContainerZone(t.Context(), "late")
		got <- z
	}()
	select {
	case z := <-got:
		t.Fatalf("the agent took zone %+v of a container the CLI is reading before a list carried it", z)
	case <-waits:
	}
	p.apply(Snapshot{Source: "s", Seq: 2, Containers: []SnapshotContainer{
		{ID: "late", Image: "app:1", IPs: []string{"172.17.0.4"}, Fingerprint: fpDefault, Zone: &Zone{Name: "UTC"}},
	}})
	if z := <-got; z.Name != "UTC" {
		t.Fatalf("the zone once listed = %+v", z)
	}
}

// TestAListAfterTheReadKeepsTheAppZone: a list that answers after an app
// container's read (its placing's) keeps the zone the read put in, as it
// keeps its fingerprint: the list carries no configuration, and gives the
// agent no new list.
func TestAListAfterTheReadKeepsTheAppZone(t *testing.T) {
	berlin := zoneData(t, "Europe/Berlin")
	f := newFakeDocker()
	r, applied, placed := watchingPlaced(t, f)
	g := make(chan struct{})
	defer close(g)
	f.mu.Lock()
	f.placeGate["svc"] = g
	f.zoneFiles = map[string]*fakeZoneAPI{"svc": {t: t, files: map[string]zoneFile{"/etc/localtime": {link: "/usr/share/zoneinfo/Europe/Berlin"}, "/usr/share/zoneinfo/Europe/Berlin": {data: berlin}}}}
	f.mu.Unlock()
	f.set("svc", appContainer("172.17.0.90"))
	f.msgs <- start("svc")
	<-f.placeEntered // its list is held
	for id := range applied {
		if id == "svc" {
			break
		}
	}
	zoneOf := func() *Zone {
		s, _, _ := r.snapshot("")
		for _, c := range s.Containers {
			if c.ID == "svc" {
				return c.Zone
			}
		}
		return nil
	}
	if z := zoneOf(); z == nil || z.Name != "Europe/Berlin" {
		t.Fatalf("the read did not index the app container's zone: %+v", z)
	}
	r.mu.Lock()
	gen := r.changeGen
	r.mu.Unlock()
	g <- struct{}{} // the list answers after the read, at the same event
	for id := <-placed; id != "svc"; id = <-placed {
	}
	if z := zoneOf(); z == nil || z.Name != "Europe/Berlin" {
		t.Fatalf("the list after the read dropped the app container's zone: %+v", z)
	}
	r.mu.Lock()
	again := r.changeGen - gen
	r.mu.Unlock()
	if again != 0 {
		t.Fatalf("the list, the same as the read's entry, counted %d change(s): a docker-mode agent is given the same list again", again)
	}
	// Restarted, its list held until its read is indexed and then let
	// through: the changes counted, which the agent is given lists at.
	restartWith := func(files map[string]zoneFile) (uint64, *Zone) {
		f.mu.Lock()
		f.zoneFiles["svc"] = &fakeZoneAPI{t: t, files: files}
		f.mu.Unlock()
		r.mu.Lock()
		gen := r.changeGen
		r.mu.Unlock()
		f.msgs <- start("svc")
		<-f.placeEntered
		for id := range applied {
			if id == "svc" {
				break
			}
		}
		g <- struct{}{}
		for id := <-placed; id != "svc"; id = <-placed {
		}
		r.mu.Lock()
		n := r.changeGen - gen
		r.mu.Unlock()
		return n, zoneOf()
	}
	restart := func(name string, data []byte) (uint64, *Zone) {
		return restartWith(map[string]zoneFile{"/etc/localtime": {link: "/usr/share/zoneinfo/" + name}, "/usr/share/zoneinfo/" + name: {data: data}})
	}
	same, z := restart("Europe/Berlin", berlin)
	if z == nil || z.Name != "Europe/Berlin" {
		t.Fatalf("a restart in the same zone indexed the app container's zone as %+v", z)
	}
	// Restarted with another zone, and the same configuration: the read
	// that finds it changes the list the agent is given, once more than a
	// restart that changed nothing, so the agent is never left holding the
	// zone of before.
	other, z := restart("Asia/Kolkata", zoneData(t, "Asia/Kolkata"))
	if z == nil || z.Name != "Asia/Kolkata" {
		t.Fatalf("the read after a restart did not index the container's new zone: %+v", z)
	}
	if other != same+1 {
		t.Fatalf("a restart that changed only the app container's zone counted %d change(s), one that changed nothing %d: the agent is not given the list of its new zone", other, same)
	}
	// A copied /etc/localtime of no /etc/timezone is a zone of no name:
	// restarted with another copied file, the name ("") is the same, the
	// rules are not, and the agent is given them.
	copied := func(data []byte) map[string]zoneFile {
		return map[string]zoneFile{"/etc/localtime": {data: data}}
	}
	if _, z := restartWith(copied(berlin)); z == nil || z.Name != "" || !bytes.Equal(z.Data, berlin) {
		t.Fatalf("a restart with a copied Berlin file indexed the app container's zone as %+v; want unnamed, Berlin's bytes", z)
	}
	sameCopy, _ := restartWith(copied(berlin))
	kolkata := zoneData(t, "Asia/Kolkata")
	otherCopy, z := restartWith(copied(kolkata))
	if z == nil || z.Name != "" || !bytes.Equal(z.Data, kolkata) {
		t.Fatalf("a restart with a copied Kolkata file indexed the app container's zone as %+v; want unnamed, Kolkata's bytes", z)
	}
	if otherCopy != sameCopy+1 {
		t.Fatalf("a restart that changed only the rules of an unnamed zone counted %d change(s), one that changed nothing %d: the agent keeps the rules of before", otherCopy, sameCopy)
	}
}
