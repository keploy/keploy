package telemetry

type TelemetryStore interface {
	ExtractInstallationId(bool) error
	GenerateTelemetryConfigFile(string) error
}
