package fs

import (
	"log"
	"os"
	"path/filepath"
)

type LogExportIO struct {
	path   string
	stream chan []byte
}

func NewLogExportIO(path string) *LogExportIO {
	return &LogExportIO{path, nil}
}

func (io *LogExportIO) OpenStream() {
	stream := make(chan []byte)

	// works on unix or windows
	cleanPath := filepath.Clean(io.path)

	file, err := os.OpenFile(cleanPath, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatal("Error creating file " + file.Name() + err.Error())
	}

	go func() {
		defer file.Close()
		for data := range stream {
			if _, err := file.Write(data); err != nil {
				log.Fatal("Error writing to file " + file.Name() + err.Error())
				break
			}
		}

	}()
}

func (io *LogExportIO) Write(msg string) {
	io.stream <- []byte(msg)
}
