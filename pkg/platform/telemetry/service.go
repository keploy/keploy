// Package telemetry provides functionality for telemetry services.
package telemetry

type TeleDB interface {
	Get(bool) (string, error)
	Set(string) error
}
