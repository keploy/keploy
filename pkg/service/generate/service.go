package generate

type Generator interface {
	Generate(configPath string)
}
