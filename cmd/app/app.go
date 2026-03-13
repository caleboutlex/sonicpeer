package app

import (
	"os"

	"gopkg.in/urfave/cli.v1"
)

// Run starts the sonicpeer application.
func Run() error {
	return RunWithArgs(os.Args, nil)
}

// AppControl is a struct of channels facilitating the interaction of a test
// harness with a sonicpeer application instance.
type AppControl struct {
	// Upon a successful start of the node, the node ID is sent to this
	// channel. The channel is closed when the process stops.
	NodeIdAnnouncement chan<- string
	// Upon a successful start of the node, the endpoint URL of the HTTP
	// server is sent to this channel. The channel is closed when the process stops.
	HttpPortAnnouncement chan<- string
	// The process is stopped by sending a message through this channel, or by
	// closing it.
	Shutdown <-chan struct{}
}

// RunWithArgs starts sonicpeer with the given command line arguments.
func RunWithArgs(args []string, control *AppControl) error {
	app := initApp()

	// If present, take ownership and inject the control struct into the action.
	if control != nil {
		if control.NodeIdAnnouncement != nil {
			defer close(control.NodeIdAnnouncement)
		}
		if control.HttpPortAnnouncement != nil {
			defer close(control.HttpPortAnnouncement)
		}
		app.Action = func(ctx *cli.Context) error {
			return sentryMainInternal(ctx, control)
		}
	}
	return app.Run(args)
}
