package main

import (
	"encoding/base64"
	"fmt"
)

func main() {
	encoded := "AAAAAAAAAAAAAP//wKgJEA=="
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		fmt.Println("Failed to decode base64:", err)
		return
	}
	fmt.Printf("Decoded data in hexadecimal: %x\n", data)
}

