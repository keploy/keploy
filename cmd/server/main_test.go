package main

import (
	"github.com/keploy/go-sdk/keploy"
	"os"
	"testing"
)

func TestKeploy(t *testing.T) {
	os.Setenv("KEPLOY_APP_NAME", "Keploy-Test-App-2")
	keploy.SetTestMode()
	go main()
	keploy.AssertTests(t)
}
