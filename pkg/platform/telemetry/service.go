package telemetry

type FS interface {
	Get(bool) (string, error)
	Set(string) error
}

