//go:build !linux

package main

import "fmt"

func main() {
	fmt.Println("containerd-shim-persistent-v2 requires Linux")
}
