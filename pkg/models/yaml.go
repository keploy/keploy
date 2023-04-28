package models

import "os"

type YamlHandler interface {
	Write(path string, obj interface{}) error
	Read(path string, obj interface{}) error
	ReadDir(path string) ([]os.DirEntry, error)
}
