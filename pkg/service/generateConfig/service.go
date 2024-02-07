package generateConfig

type GeneratorConfig interface {
	GenerateConfig(path string, options GenerateConfigOptions)
}
