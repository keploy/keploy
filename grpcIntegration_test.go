package main

import (
	"testing"
)

var Emoji = "\U0001F430" + " Keploy:"

func TestTestMode(t *testing.T) {
	codeCoverTestMode(t)
	// assertions after running keploy
}

func TestRecordMode(t *testing.T) {
	codeCoverRecordMode(t)
}
