package main

import (
	"fmt"
	"runtime"
)

func main() {
	fmt.Println("hello build!")
	fmt.Println("architecture:", runtime.GOARCH)
}
