package telemetry

type TeleDB interface {
	Get(bool) (string, error)
	Set(string) error
}
