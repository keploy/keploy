// Command daemonapp is the fixture of the daemonized-members lane: an app
// whose request handler starts a daemon the way pg_ctl start and
// redis-server --daemonize do (a child that starts a grandchild in a session
// of its own and exits, so the grandchild is orphaned and reparented), and the
// daemon then calls an upstream. It also calls the upstream itself and through
// a child that is not daemonized, as controls.
//
//	daemonapp upstream ADDR LOG   serve ADDR, appending each request's path to LOG
//	daemonapp app ADDR UPSTREAM   serve ADDR: GET /go makes the three calls
//	daemonapp daemonize URL DONE  start "get" in a new session, and exit
//	daemonapp get URL [DONE PID]  GET URL; with DONE, once PID is no longer its
//	                              parent, and then write DONE
//	daemonapp daemonize-serve ADDR LOG READY
//	                              start "serve" in a new session, and exit
//	daemonapp serve ADDR LOG READY
//	                              serve ADDR as a daemon does: write READY
//	                              once it listens, answer GET /hi with "hi",
//	                              appending each path to LOG
package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 3 || (os.Args[1] != "get" && len(os.Args) < 4) {
		fmt.Fprintln(os.Stderr, "usage: daemonapp upstream|app|daemonize|get ...")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "upstream":
		upstream(os.Args[2], os.Args[3])
	case "app":
		app(os.Args[2], os.Args[3])
	case "daemonize":
		daemonize(os.Args[2], os.Args[3])
	case "get":
		get(os.Args[2:])
	case "daemonize-serve":
		detach(append([]string{"serve"}, os.Args[2:]...)...)
	case "serve":
		serve(os.Args[2], os.Args[3], os.Args[4])
	default:
		os.Exit(2)
	}
}

func upstream(addr, log string) {
	f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		panic(err)
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(f, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
	})
	panic(http.ListenAndServe(addr, nil))
}

func app(addr, up string) {
	self, err := os.Executable()
	if err != nil {
		panic(err)
	}
	// DAEMON_SRV: the app starts a server daemon as it starts, as an app
	// whose start runs pg_ctl start does, and waits until it listens; one
	// that cannot (a daemon of an earlier run still holds the port) fails
	// the app's start.
	srv := os.Getenv("DAEMON_SRV")
	if srv != "" {
		ready := fmt.Sprintf("%s/daemon-srv-ready-%d", os.TempDir(), os.Getpid())
		if err := exec.Command(self, "daemonize-serve", srv, os.DevNull, ready).Run(); err != nil {
			panic(err)
		}
		for {
			if b, err := os.ReadFile(ready); err == nil && len(b) > 0 {
				_ = os.Remove(ready)
				if strings.HasPrefix(string(b), "listen") {
					fmt.Fprintln(os.Stderr, "the server daemon could not start: "+string(b))
					os.Exit(1)
				}
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	http.HandleFunc("/go", func(w http.ResponseWriter, r *http.Request) {
		var out []string
		// The app's own call.
		out = append(out, "app: "+call("http://"+up+"/from-app"))
		// A child's call: a member however the walk is done.
		child, err := exec.Command(self, "get", "http://"+up+"/from-child").CombinedOutput()
		out = append(out, "child: "+strings.TrimSpace(string(child))+errText(err))
		// The daemon's call: made by an orphan of the app's tree, after its
		// parent exited. Its result is not in the answer: a call that escaped
		// keploy changes nothing the test compares, so only keploy can tell.
		done := fmt.Sprintf("%s/daemon-done-%d", os.TempDir(), time.Now().UnixNano())
		if err := exec.Command(self, "daemonize", "http://"+up+"/from-daemon", done).Run(); err != nil {
			http.Error(w, "daemonize: "+err.Error(), http.StatusInternalServerError)
			return
		}
		for {
			if _, err := os.Stat(done); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		b, _ := os.ReadFile(done)
		_ = os.Remove(done)
		fmt.Fprintf(os.Stderr, "daemon said: %s\n", b)
		out = append(out, "daemon: done")
		if srv != "" {
			out = append(out, "server: "+call("http://"+srv+"/hi"))
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, strings.Join(out, "\n"))
	})
	panic(http.ListenAndServe(addr, nil))
}

// daemonize starts get in a session of its own and exits without waiting for
// it: get is orphaned, and the kernel reparents it to the nearest subreaper
// or to init.
func daemonize(url, done string) {
	self, err := os.Executable()
	if err != nil {
		panic(err)
	}
	cmd := exec.Command(self, "get", url, done, fmt.Sprint(os.Getpid()))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		panic(err)
	}
	os.Exit(0)
}

// get, run by daemonize, waits until daemonize (pid) is no longer its parent,
// as it is not once daemonize has exited and the kernel reparented get, so
// that its call is an orphan's; run by the app, it calls at once.
func get(args []string) {
	url := args[0]
	if len(args) < 3 {
		fmt.Println(call(url))
		return
	}
	done, parent := args[1], args[2]
	for fmt.Sprint(os.Getppid()) == parent {
		time.Sleep(10 * time.Millisecond)
	}
	res := call(url)
	ppid := os.Getppid()
	exe, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", ppid))
	_ = os.WriteFile(done+".tmp", []byte(fmt.Sprintf("ppid %d (exe %s): %s", ppid, exe, res)), 0o644)
	_ = os.Rename(done+".tmp", done)
}

// detach starts this binary with args in a session of its own and exits
// without waiting for it, as pg_ctl start does.
func detach(args ...string) {
	self, err := os.Executable()
	if err != nil {
		panic(err)
	}
	cmd := exec.Command(self, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		panic(err)
	}
	os.Exit(0)
}

func serve(addr, log, ready string) {
	if os.Getenv("DEAF") == "1" {
		signal.Ignore(syscall.SIGTERM) // a daemon that does not stop when asked
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// Said where the command that started it waits: a daemon of an
		// earlier run still holding the port is what the lane checks for.
		_ = os.WriteFile(ready, []byte("listen failed: "+err.Error()), 0o644)
		os.Exit(1)
	}
	f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(ready, []byte(fmt.Sprint(os.Getpid())), 0o644); err != nil {
		panic(err)
	}
	panic(http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(f, r.URL.Path)
		fmt.Fprint(w, "hi")
	})))
}

func call(url string) string {
	resp, err := http.Get(url)
	if err != nil {
		return "error: " + err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.Status + " " + string(b)
}

func errText(err error) string {
	if err != nil {
		return " (" + err.Error() + ")"
	}
	return ""
}
