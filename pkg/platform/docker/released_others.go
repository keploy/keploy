//go:build !linux

package docker

// MarkAppReleased writes nothing off Linux: no pod runs the agent there.
func MarkAppReleased(string) error { return nil }

// AppReleasedInThisPod is false off Linux: no pod runs the agent there.
func AppReleasedInThisPod(string) (bool, error) { return false, nil }
