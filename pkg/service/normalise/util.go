package Normalise

import "os"

// getDirectories returns a list of directories in the given path.
func getDirectories(path string) ([]string, error) {
	var dirs []string

	// Open the directory
	dir, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer dir.Close()

	// Read the directory entries
	fileInfos, err := dir.Readdir(-1)
	if err != nil {
		return nil, err
	}

	// Filter directories
	for _, fileInfo := range fileInfos {
		if fileInfo.IsDir() {
			dirs = append(dirs, fileInfo.Name())
		}
	}

	return dirs, nil
}
