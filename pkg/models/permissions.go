package models

import "os"

const (
	// Owner: read+write, Group: read, Others: read
	FilePermReadWrite = os.FileMode(0644)

	// Owner: read+write, Group: none, Others: none
	FilePermPrivate = os.FileMode(0600)

	// Owner: read+write+execute, Group: read+execute, Others: read+execute
	DirPermDefault = os.FileMode(0755)

	// Owner: read+write+execute, Group: none, Others: none
	DirPermPrivate = os.FileMode(0700)
)
