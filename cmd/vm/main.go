package main

import (
	"log"

	"github.com/bubunyo/monolith"
)

func main() {
	if err := monolith.Run("."); err != nil {
		log.Fatal(err)
	}
}
