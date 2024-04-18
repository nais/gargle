package main

import (
	"context"
	"fmt"
	"os"

	"github.com/nais/gargle/internal/gargle"
)

func main() {
	ctx := context.Background()
	if err := gargle.Main(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
