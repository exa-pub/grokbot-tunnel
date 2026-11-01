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

// stopSignals stop the program cleanly. SIGHUP comes when the terminal of a
// foreground connect goes away, e.g. with its SSH session.
var stopSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), stopSignals...)
	err := app.Run(ctx, os.Args[1:])
	stop()
	if err != nil {
		log.Print(err)
		os.Exit(app.ExitCode(err))
	}
}
