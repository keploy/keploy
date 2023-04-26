package fs

import (
	"fmt"
	"gopkg.in/yaml.v3"
	"log"
	"os"
	"path/filepath"
)

type YamlHandlerImpl struct{}

// Write will use yaml serializer to write a given go obj to the file specified
// at path field of the struct. It's important to note that if the file is not
// empty it will append the object on it
func (yh *YamlHandlerImpl) Write(path string, obj interface{}) (bool, error) {

	var err error
	var file *os.File

	if !yh.Exists(path) {
		file, err = yh.createFile(path)
		if err != nil {
			return false, err
		}
	} else {
		file, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, os.ModePerm)
		if err != nil {
			return false, err
		}
	}

	data, err := yaml.Marshal(obj)
	if err != nil {
		return false, err
	}

	_, err = file.Write(data)
	if err != nil {
		return false, err
	}

	return true, nil
}

// Read will return a go object that represents the fields defined
// in the YAML. It's an array of interface because yamls are divided
// by "---", so it's almost like we have another yaml inside the same.
// Filter is the condition it must have to be added in the array
func (yh *YamlHandlerImpl) Read(path string, obj interface{}) error {

	file, err := os.OpenFile(path, os.O_RDONLY, os.ModePerm)
	if err != nil {
		return err
	}

	defer func(file *os.File) {
		var err = file.Close()
		if err != nil {
			log.Println("Error closing file: " + err.Error())
		}
	}(file)

	decoder := yaml.NewDecoder(file)

	err = decoder.Decode(&obj)
	if err != nil {
		return fmt.Errorf("failed to decode the yaml file document. error: %v", err.Error())
	}

	return nil
}

// ReadDir will read a directory and returns list of os.DirEntry that has the
// correct .yaml exit
func (yh *YamlHandlerImpl) ReadDir(path string) ([]os.DirEntry, error) {
	dir, err := os.OpenFile(path, os.O_RDONLY, os.ModePerm)
	if err != nil {
		return nil, err
	}

	dirInfo, err := dir.ReadDir(0)
	if err != nil {
		return nil, err
	}

	var filtered []os.DirEntry
	for _, e := range dirInfo {
		if filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		filtered = append(filtered, e)
	}

	return dirInfo, nil
}
func (yh *YamlHandlerImpl) Exists(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return true
	}
	return false
}

func (yh *YamlHandlerImpl) createFile(path string) (*os.File, error) {

	err := os.MkdirAll(filepath.Join(path), os.ModePerm)
	if err != nil {
		return nil, err
	}

	file, err := os.Create(path)

	if err != nil {
		return nil, err
	}

	return file, nil
}
