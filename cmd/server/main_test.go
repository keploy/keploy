package main

import (
	"fmt"
	"github.com/keploy/go-sdk/keploy"
	"os"
	// "path"
	"path/filepath"
	"runtime"
	"testing"
)

func MakeFunctionRunOnRootFolder() {
	_, filename, _, _ := runtime.Caller(0)
	fmt.Println("    filename: ", filename)
	// The ".." may change depending on you folder structure
	dir, err := filepath.Abs("../../")
	if err != nil {
		panic(err)
	}
	// fmt.Println("    dir : ", path.Join(path.Dir(filename), "../"), dir)

	err = os.Chdir(dir)
	if err != nil {
		panic(err)
	}
	// _, filename, _, _ = runtime.Caller(0)
	// path, err := os.Getwd()
	// if err != nil {
	// 	panic(err)
	// }
	// fmt.Println("    filename after: ", path)

}

func TestKeploy(t *testing.T) {
	// setup
	MakeFunctionRunOnRootFolder()
	keploy.SetTestMode()
	os.Setenv("ENABLE_TELEMETRY", "false")
	os.Setenv("ENABLE_TEST_EXPORT", "false")

	// run testcases
	go main()

	// publish test result
	keploy.AssertTests(t)
}
