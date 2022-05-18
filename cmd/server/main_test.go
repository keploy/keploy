package main

import (
	"github.com/keploy/go-sdk/keploy"
	"testing"
)

func TestKeploy(t *testing.T) {
	keploy.SetTestMode()
	go main()
	keploy.AssertTests(t)
}
