/*
* The purpose of this file is to serialize yaml as well store it in the filesystem.
* It was purposively written with the small number of methods, so we can mock it if
* while doing tests.
 */

package fs

import (
	"gopkg.in/yaml.v3"
	"log"
	"os"
	"path/filepath"
)

const YamlExt = ".yaml"

type YamlHandlerImpl struct {
	decoderCache *yaml.Decoder
}

func NewYamlHandlerImpl() *YamlHandlerImpl {
	return &YamlHandlerImpl{}
}

// Write will use the yaml serializer to write a given go obj to the file specified
// at path field of the struct. It's important to note that if the file is not
// empty it will just append the object on it. Note that there's no need to write
// the .yaml at the end as this method already do it.
func (yh *YamlHandlerImpl) Write(path string, obj interface{}) error {

	var err error
	var file *os.File
	data := []byte("")

	if !exists(path) {
		file, err = createFile(path + YamlExt)
		if err != nil {
			return err
		}
	} else {
		file, err = os.OpenFile(path+YamlExt, os.O_CREATE|os.O_WRONLY|os.O_APPEND, os.ModePerm)
		if err != nil {
			return err
		}
		data = []byte("---\n")
	}
	// Close file
	defer func(file *os.File) {
		err := file.Close()
		if err != nil {
			log.Println("Error closing file: " + err.Error()) //todo: change to Keploy's default logger
		}
	}(file)

	objBytes, err := yaml.Marshal(obj)
	data = append(data, objBytes...)
	if err != nil {
		return err
	}

	_, err = file.Write(data)
	if err != nil {
		return err
	}

	return nil
}

// Read will read a yaml path and deserialize it at the given obj
func (yh *YamlHandlerImpl) Read(path string, obj interface{}) error {

	file, err := os.OpenFile(path+YamlExt, os.O_RDONLY, os.ModePerm)
	if err != nil {
		return err
	}

	// When reading yaml files that has more than one yamls inside itself separated
	// by "---" we need to keep the decoder "open" so we don't end up decoding the same
	// yaml part everytime
	if yh.decoderCache == nil {
		yh.decoderCache = yaml.NewDecoder(file)
	}

	err = yh.decoderCache.Decode(&obj)
	if err != nil {
		yh.decoderCache = nil

		defer func(file *os.File) {
			var err = file.Close()
			if err != nil {
				log.Println("Error closing file: " + err.Error())
			}
		}(file)
		return err
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
		if filepath.Ext(e.Name()) != YamlExt {
			continue
		}
		filtered = append(filtered, e)
	}

	return dirInfo, nil
}

func exists(path string) bool {
	if _, err := os.Stat(path + YamlExt); err != nil {
		return false
	}
	return true
}

func createFile(path string) (*os.File, error) {
	dir := filepath.Dir(path)

	if !exists(dir) {
		err := os.MkdirAll(dir, os.ModePerm)
		if err != nil {
			return nil, err
		}
	}

	file, err := os.Create(path)

	if err != nil {
		return nil, err
	}

	return file, nil
}
