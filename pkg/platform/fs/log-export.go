package fs

import (
	"log"
	"os"
	"path/filepath"
)

type LogExport struct {
	path   string
	stream chan []byte
}

// OpenStream is the method responsible for open a channel where anything written
// into it will be written into a file at given path, for example:
// /home/user/out.txt also this function works for both Windows and Unix like
// paths
func OpenStream(path string) (*LogExport, error) {
	logExport := LogExport{path, make(chan []byte)}

	// works on unix or windows
	cleanPath := filepath.Clean(path)

	file, err := os.OpenFile(cleanPath, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}

	go func() {

		// Close file
		defer func(file *os.File) {
			err := file.Close()
			if err != nil {
				_, _ = logExport.Write([]byte("Error closing file"))
			}
		}(file)

		// Read from channel
		for data := range logExport.stream {
			if _, err := file.Write(data); err != nil {
				log.Fatal("Error writing to file " + file.Name() + err.Error())
			}
		}
	}()

	return &logExport, nil
}

// Sync serves to implement the zapcore.WriteSyncer interface
func (io *LogExport) Sync() error {
	close(io.stream)
	return nil
}

func (io *LogExport) Write(p []byte) (int, error) {
	n := len(p)
	io.stream <- p
	return n, nil
}

func (io *LogExport) FileName() string {
	return io.path
}
