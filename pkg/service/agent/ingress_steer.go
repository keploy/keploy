package agent

// IngressSteers reports whether this agent's hooks steer the application's
// ports instead of moving its binds (the Linux hooks' IngressSteers): the
// application keeps its port, and the agent's death leaves it to the
// application. False for hooks that cannot.
func (a *Agent) IngressSteers() bool {
	s, ok := a.Hooks.(interface{ IngressSteers() bool })
	return ok && s.IngressSteers()
}
