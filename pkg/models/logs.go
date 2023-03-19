package models

type LogLocation string

const (
	// Console will only write the logs to the console.
	Console = LogLocation("console")

	// File will only write the logs to the file mentioned by the user.
	// If the file hasn't been specified, a default one is used instead.
	File = LogLocation("file")

	// Pipe would write the logs to the console as well as on file.
	// If the file hasn't been specified, a default one is used instead.
	Pipe = LogLocation("pipe")
)
