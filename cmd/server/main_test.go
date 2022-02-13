package main
import (
	"testing"
	"github.com/keploy/go-sdk/keploy"
)

func TestKeploy(t *testing.T)  {
	keploy.SetTestMode()
	go main()
	keploy.AssertTests(t)
}