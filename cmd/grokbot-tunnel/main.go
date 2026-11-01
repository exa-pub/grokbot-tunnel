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

// stopSignals returns the signals that stop the program cleanly. SIGHUP comes
// when the terminal of a foreground connect goes away, e.g. with its SSH
// session. It is left out if the program was started with it ignored, as by
// nohup: handling it would end the ignoring.
func stopSignals() []os.Signal {
	sigs := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if !signal.Ignored(syscall.SIGHUP) {
		sigs = append(sigs, syscall.SIGHUP)
	}
	return sigs
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), stopSignals()...)
	err := app.Run(ctx, os.Args[1:])
	stop()
	if err != nil {
		log.Print(err)
		os.Exit(app.ExitCode(err))
	}
}
