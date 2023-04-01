package models

type LogExportIO interface {
	OpenStream()
	Write(msg string)
}
