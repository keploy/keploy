package fs

import (
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"log"
	"os"
	"path/filepath"
)

type YamlHandler struct{}

// Write will use yaml serializer to write a given go obj to the file specified
// at path field of the struct. It's important to note that if the file is not
// empty it will append the object on it
func (yh *YamlHandler) Write(path string, obj interface{}) (bool, error) {

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
func (yh *YamlHandler) Read(path string, filter func() bool) ([]interface{}, error) {

	file, err := os.OpenFile(path, os.O_RDONLY, os.ModePerm)
	if err != nil {
		return nil, err
	}

	defer func(file *os.File) {
		var err = file.Close()
		if err != nil {
			log.Println("Error closing file: " + err.Error())
		}
	}(file)

	decoder := yaml.NewDecoder(file)
	var arr []interface{}
	for {
		var doc interface{}
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to decode the yaml file documents. error: %v", err.Error())
		}
		if filter() {
			arr = append(arr, doc)
		}
	}
	return arr, nil
}

func (yh *YamlHandler) Exists(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return true
	}
	return false
}

func (yh *YamlHandler) createFile(path string) (*os.File, error) {

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
