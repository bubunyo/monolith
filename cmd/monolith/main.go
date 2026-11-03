package main

import (
	"github.com/bubunyo/monolith/cli"
	_ "github.com/bubunyo/monolith/internal/termquiet" // before bubbletea's init; see the package doc
)

func main() {
	cli.Execute()
}
