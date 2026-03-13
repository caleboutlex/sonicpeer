package app

import (
	"os"
	"os/signal"
	"strings"
	"syscall"

	"sonicpeer/pkg/gossip"

	"github.com/0xsoniclabs/sonic/config/flags"
	"github.com/0xsoniclabs/sonic/debug"
	"github.com/Fantom-foundation/lachesis-base/utils/cachescale"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/discover/discfilter"
	"gopkg.in/urfave/cli.v1"
)

var (
	// customDataDirFlag is a copy of flags.DataDirFlag with a default value for the sentry node.
	datadirFlag = cli.StringFlag{Name: flags.DataDirFlag.Name, Usage: flags.DataDirFlag.Usage, Value: "./node_data"}
)

var appFlags = []cli.Flag{
	// Sentry-specific flags
	cli.Uint64Flag{
		Name:  "networkId",
		Usage: "Network identifier",
		Value: 146,
	},
	cli.StringFlag{
		Name:  "genesis",
		Usage: "Genesis hash",
		Value: "0xdbec022b6a5b0a15c902cf16620d2048f69e9b4cb0dd5d6b57edcf842b85494a",
	},
	cli.StringFlag{
		Name:  "url",
		Usage: "URL of the backend validator RPC",
		Value: "ws://192.168.0.115:18546",
	},

	// Node flags (from go-ethereum and sonic)
	datadirFlag,
	flags.BootnodesFlag,
	flags.ListenPortFlag,
	flags.MaxPeersFlag,

	// RPC flags
	flags.HTTPEnabledFlag,
	flags.HTTPListenAddrFlag,
	flags.HTTPPortFlag,
	flags.HTTPCORSDomainFlag,
	flags.HTTPVirtualHostsFlag,
	flags.HTTPApiFlag,
	flags.HTTPPathPrefixFlag,
	flags.WSEnabledFlag,
	flags.WSListenAddrFlag,
	flags.WSPortFlag,
	flags.WSApiFlag,
	flags.WSAllowedOriginsFlag,
	flags.WSPathPrefixFlag,
	flags.IPCDisabledFlag,
}

func initApp() *cli.App {
	discfilter.Enable()

	app := cli.NewApp()
	app.Action = sentryMain
	app.Name = "sonicpeer"
	app.Usage = "Sonic MEV Node"
	app.Version = "1.0.0"

	app.Flags = append(app.Flags, appFlags...)
	app.Flags = append(app.Flags, debug.Flags...)

	app.Before = func(ctx *cli.Context) error {
		if err := debug.Setup(ctx); err != nil {
			return err
		}
		return nil
	}
	return app
}

// sentryMain is the main entry point into the system if no special sub-command is ran.
func sentryMain(ctx *cli.Context) error {
	return sentryMainInternal(ctx, nil)
}

// sentryMainInternal is the internal entry point that allows for test control.
func sentryMainInternal(ctx *cli.Context, control *AppControl) error {
	// Populate the SentryConfig from CLI flags
	gossipCfg := gossip.DefaultConfig(cachescale.Identity)
	sentryCfg := gossip.SentryConfig{
		BackendURL:       ctx.GlobalString("url"),
		NetworkID:        ctx.GlobalUint64("networkId"),
		Genesis:          common.HexToHash(ctx.GlobalString("genesis")),
		DataDir:          ctx.GlobalString(datadirFlag.Name),
		Bootnodes:        splitAndTrim(ctx.GlobalString(flags.BootnodesFlag.Name)),
		ListenPort:       ctx.GlobalInt(flags.ListenPortFlag.Name),
		MaxPeers:         ctx.GlobalInt(flags.MaxPeersFlag.Name),
		HTTPEnabled:      ctx.GlobalBool(flags.HTTPEnabledFlag.Name),
		HTTPListenAddr:   ctx.GlobalString(flags.HTTPListenAddrFlag.Name),
		HTTPPort:         ctx.GlobalInt(flags.HTTPPortFlag.Name),
		HTTPCORSDomain:   splitAndTrim(ctx.GlobalString(flags.HTTPCORSDomainFlag.Name)),
		HTTPVirtualHosts: splitAndTrim(ctx.GlobalString(flags.HTTPVirtualHostsFlag.Name)),
		HTTPApi:          splitAndTrim(ctx.GlobalString(flags.HTTPApiFlag.Name)),
		HTTPPathPrefix:   ctx.GlobalString(flags.HTTPPathPrefixFlag.Name),
		WSEnabled:        ctx.GlobalBool(flags.WSEnabledFlag.Name),
		WSListenAddr:     ctx.GlobalString(flags.WSListenAddrFlag.Name),
		WSPort:           ctx.GlobalInt(flags.WSPortFlag.Name),
		WSApi:            splitAndTrim(ctx.GlobalString(flags.WSApiFlag.Name)),
		WSAllowedOrigins: splitAndTrim(ctx.GlobalString(flags.WSAllowedOriginsFlag.Name)),
		WSPathPrefix:     ctx.GlobalString(flags.WSPathPrefixFlag.Name),
		IPCDisabled:      ctx.GlobalBool(flags.IPCDisabledFlag.Name),
		GossipConfig:     gossipCfg,
	}

	sentry, err := gossip.NewSentry(sentryCfg)
	if err != nil {
		return err
	}

	if err := startSentry(sentry, control); err != nil {
		return err
	}

	sentry.Wait()
	log.Info("Sonic  Sentry Node stopped")
	return nil
}

func startSentry(sentry *gossip.Sentry, control *AppControl) error {
	if err := sentry.Start(); err != nil {
		log.Error("Failed to start sentry", "err", err)
		return err
	}
	log.Info("Sonic Sentry Node started")

	// Announce node info for testing harness
	if control != nil {
		if control.NodeIdAnnouncement != nil {
			control.NodeIdAnnouncement <- sentry.NodeInfo().String()
		}
		if control.HttpPortAnnouncement != nil && sentry.HTTPEndpoint() != "" {
			control.HttpPortAnnouncement <- sentry.HTTPEndpoint()
		}
	}

	go func() {
		sigc := make(chan os.Signal, 1)
		signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(sigc)

		var stopChan <-chan struct{}
		if control != nil {
			stopChan = control.Shutdown
		}

		select {
		case <-sigc:
			log.Info("Got interrupt, shutting down...")
		case <-stopChan:
			log.Info("Got stop signal, shutting down...")
		}

		go sentry.Stop()
		for i := 10; i > 0; i-- {
			<-sigc
			if i > 1 {
				log.Warn("Already shutting down, interrupt more to panic.", "times", i-1)
			}
		}
		debug.Exit()
	}()
	return nil
}

// splitAndTrim splits a comma-separated string and trims whitespace from each part.
func splitAndTrim(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	for i, part := range parts {
		parts[i] = strings.TrimSpace(part)
	}
	return parts
}
