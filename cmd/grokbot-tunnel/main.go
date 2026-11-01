// Command grokbot-tunnel connects this computer to Grok Bot.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/exa-pub/grokbot-tunnel/internal/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := app.Run(ctx, os.Args[1:])
	stop()
	if err != nil {
		log.Print(err)
		os.Exit(app.ExitCode(err))
	}
}
