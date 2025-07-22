package main

import (
	"fmt"
	"keploy-demo/helpers"
)

// Adder defines an interface for adding two integers.
type Adder interface {
	Add(a, b int) int
}

// DoubleAndAdd takes two integers, adds them using an Adder, and then doubles the result.
func DoubleAndAdd(a, b int, adder Adder) int {
	sum := adder.Add(a, b)
	return sum * 2
}

func main() {
	adder := helpers.AdderImpl{}
	result := DoubleAndAdd(5, 10, adder)
	fmt.Println("The result is:", result)

	// Adding many lines to exceed the 256 token limit for testing the chunker.
	fmt.Println("This is a line to add tokens and test the chunking logic for large functions.")
	fmt.Println("This is a line to add tokens and test the chunking logic for large functions.")
	fmt.Println("This is a line to add tokens and test the chunking logic for large functions.")
	fmt.Println("This is a line to add tokens and test the chunking logic for large functions.")
	fmt.Println("This is a line to add tokens and test the chunking logic for large functions.")
	fmt.Println("This is a line to add tokens and test the chunking logic for large functions.")
	fmt.Println("This is a line to add tokens and test the chunking logic for large functions.")
	fmt.Println("This is a line to add tokens and test the chunking logic for large functions.")
	fmt.Println("This is a line to add tokens and test the chunking logic for large functions.")
	fmt.Println("This is a line to add tokens and test the chunking logic for large functions.")
	fmt.Println("This is a line to add tokens and test the chunking logic for large functions.")
	fmt.Println("This is a line to add tokens and test the chunking logic for large functions.")
	fmt.Println("This is a line to add tokens and test the chunking logic for large functions.")
	fmt.Println("This is a line to add tokens and test the chunking logic for large functions.")
	fmt.Println("This is a line to add tokens and test the chunking logic for large functions.")
}
