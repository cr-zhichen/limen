package main

import (
	"context"
	"fmt"
	"os"

	"github.com/cr-zhichen/limen/internal/cli"
)

func main() {
	if err := cli.New().ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
