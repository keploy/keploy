package docker

// AppReleasedFile is where an agent of a pod notes that it released the pod's
// application (MarkAppReleased): the pod's /dev/shm, which every container of
// a Kubernetes pod shares (the pod's IPC namespace), which outlives a
// restarted agent container and goes with the pod, in the classic sidecar and
// the native (low-latency) one alike. The recording start and the steer table
// are kept there for the same reason (pkg/agent/hooks/linux).
//
// A restarted agent finds the application already running: it was released
// by the agent before it. The sidecar's readiness (the enterprise build's
// health --check=agent-ready) takes this note, with the restarted agent's
// own start being over, in place of a new release: nothing has to start the
// application again, and waiting on the recorder's next release held the pod
// NotReady (its Service down, for a single replica) for the whole outage.
var AppReleasedFile = "/dev/shm/keploy-app-released"
