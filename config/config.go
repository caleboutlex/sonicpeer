// Copyright 2026 Sonic Operations Ltd
// This file is part of the Sonic Client
//
// Sonic is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Sonic is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with Sonic. If not, see <http://www.gnu.org/licenses/>.

package config

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"regexp"
	"strings"

	"github.com/0xsoniclabs/sonic/config/flags"
	"github.com/0xsoniclabs/sonic/version"

	"github.com/Fantom-foundation/lachesis-base/utils/cachescale"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/naoina/toml"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"gopkg.in/urfave/cli.v1"

	"github.com/0xsoniclabs/sonic/gossip"
	"github.com/0xsoniclabs/sonic/utils/caution"
	"github.com/0xsoniclabs/sonic/utils/memory"
)

const (
	// ClientIdentifier to advertise over the network.
	ClientIdentifier = "Sonic"
)

// TomlSettings ensure that TOML keys use the same names as Go struct fields.
var TomlSettings = toml.Config{
	NormFieldName: func(rt reflect.Type, key string) string {
		return key
	},
	FieldToKey: func(rt reflect.Type, field string) string {
		return field
	},
	MissingField: func(rt reflect.Type, field string) error {
		return fmt.Errorf("field '%s' is not defined in %s", field, rt.String())
	},
}

type Config struct {
	Node  node.Config
	Opera gossip.Config
}

func LoadAllConfigs(file string, cfg *Config) (err error) {
	f, err := os.Open(file)
	if err != nil {
		return fmt.Errorf("failed to open config file %s: %w", file, err)
	}
	defer caution.CloseAndReportError(&err, f, "failed to close config file")

	err = TomlSettings.NewDecoder(bufio.NewReader(f)).Decode(cfg)
	// Add file name to errors that have a line number.
	if _, ok := err.(*toml.LineError); ok {
		err = errors.New(file + ", " + err.Error())
	}
	if err != nil {
		// This is a user-facing error, so we want to provide a clear message.
		//nolint:staticcheck // ST1005: allow capitalized error message and punctuation
		return fmt.Errorf("TOML config file error: %v.\n"+
			"Use 'dumpconfig' command to get an example config file.\n"+
			"If node was recently upgraded and a previous network config file is used, then check updates for the config file.", err)
	}
	return nil
}

func SaveAllConfigs(file string, cfg *Config) error {
	encoded, err := TomlSettings.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to encode config to TOML: %w", err)
	}
	if err := os.WriteFile(file, encoded, 0644); err != nil {
		return fmt.Errorf("failed to write config file %s: %w", file, err)
	}
	return nil
}

func setBootnodes(ctx *cli.Context, urls []string, cfg *node.Config) {
	cfg.P2P.BootstrapNodesV5 = []*enode.Node{}
	for _, url := range urls {
		if url != "" {
			hostname, modified, err := resolveHostNameInEnodeURL(url)
			if err != nil {
				log.Error("Failed to resolve hostname Bootnode", "url", url, "err", err)
				continue
			}

			node, err := enode.Parse(enode.ValidSchemes, modified)
			if err != nil {
				log.Error("Bootstrap URL invalid", "enode", modified, "err", err)
				continue
			}
			node = node.WithHostname(hostname)
			cfg.P2P.BootstrapNodesV5 = append(cfg.P2P.BootstrapNodesV5, node)
		}
	}
	cfg.P2P.BootstrapNodes = cfg.P2P.BootstrapNodesV5
}

func resolveHostNameInEnodeURL(url string) (hostname string, modified string, err error) {
	return resolveHostNameInEnodeURLInternal(url, func(hostname string) (string, error) {
		ips, err := net.LookupIP(hostname)
		if err != nil {
			return "", err
		}
		if len(ips) == 0 {
			return "", fmt.Errorf("no IPs found for hostname %v", hostname)
		}
		return ips[0].String(), nil
	})
}

var _enodeHostnameRE = regexp.MustCompile(`enode:\/\/[0-9a-f]+@([^:]+):[0-9]+`)

func resolveHostNameInEnodeURLInternal(
	url string,
	resolve func(string) (string, error),
) (
	hostname string,
	modified string,
	err error,
) {
	match := _enodeHostnameRE.FindStringSubmatch(url)
	if len(match) != 2 {
		return "", "", fmt.Errorf("failed to match enode URL")
	}
	hostname = match[1]
	ip, err := resolve(hostname)
	if err != nil {
		return "", "", fmt.Errorf("failed to resolve hostname %v: %v", hostname, err)
	}
	return hostname, strings.Replace(url, hostname, ip, 1), nil
}

func gossipConfigWithFlags(ctx *cli.Context, src gossip.Config) gossip.Config {
	cfg := src

	if ctx.GlobalIsSet(flags.RPCGlobalGasCapFlag.Name) {
		cfg.RPCGasCap = ctx.GlobalUint64(flags.RPCGlobalGasCapFlag.Name)
	}
	if ctx.GlobalIsSet(flags.RPCGlobalEVMTimeoutFlag.Name) {
		cfg.RPCEVMTimeout = ctx.GlobalDuration(flags.RPCGlobalEVMTimeoutFlag.Name)
	}
	if ctx.GlobalIsSet(flags.RPCGlobalTxFeeCapFlag.Name) {
		cfg.RPCTxFeeCap = ctx.GlobalFloat64(flags.RPCGlobalTxFeeCapFlag.Name)
	}
	if ctx.GlobalIsSet(flags.RPCGlobalTimeoutFlag.Name) {
		cfg.RPCTimeout = ctx.GlobalDuration(flags.RPCGlobalTimeoutFlag.Name)
	}
	if ctx.GlobalIsSet(flags.MaxResponseSizeFlag.Name) {
		cfg.MaxResponseSize = ctx.GlobalInt(flags.MaxResponseSizeFlag.Name)
	}
	if ctx.IsSet(flags.StructLogLimitFlag.Name) {
		cfg.StructLogLimit = ctx.GlobalInt(flags.StructLogLimitFlag.Name)
	}
	return cfg
}

const (
	// DefaultCacheSize is calculated as memory consumption in a worst case scenario with default configuration
	// Average memory consumption might be 3-5 times lower than the maximum
	DefaultCacheSize  = 6 * 1024 // MB
	ConstantCacheSize = 400      // MB
)

func cacheScaler(ctx *cli.Context) cachescale.Func {
	baseSize := DefaultCacheSize
	totalMemory := int(memory.TotalMemory() / opt.MiB)
	maxCache := totalMemory * 3 / 5 // max 60% of available memory
	if maxCache < baseSize {
		maxCache = baseSize
	}
	if !ctx.GlobalIsSet(flags.CacheFlag.Name) {
		recommendedCache := totalMemory / 2
		if recommendedCache > baseSize {
			log.Warn(fmt.Sprintf("Please add '--%s %d' flag to allocate more cache for Sonic. Total memory is %d MB.", flags.CacheFlag.Name, recommendedCache, totalMemory))
		}
		return cachescale.Identity
	}
	targetCache := ctx.GlobalInt(flags.CacheFlag.Name)
	if targetCache < baseSize {
		log.Crit("Invalid flag", "flag", flags.CacheFlag.Name, "err", fmt.Sprintf("minimum cache size is %d MB", baseSize))
	}
	if totalMemory != 0 && targetCache > maxCache {
		log.Warn(fmt.Sprintf("Requested cache size exceeds 60%% of available memory. Reducing cache size to %d MB.", maxCache))
		targetCache = maxCache
	}

	return cachescale.Ratio{
		Base:   uint64(baseSize - ConstantCacheSize),
		Target: uint64(targetCache - ConstantCacheSize),
	}
}

func MakeAllConfigsFromFile(ctx *cli.Context, configFile string) (*Config, error) {
	// Defaults (low priority)
	cacheRatio := cacheScaler(ctx)
	cfg := Config{
		Node:  DefaultNodeConfig(),
		Opera: gossip.DefaultConfig(cacheRatio),
	}

	// "asDefault" means set network defaults
	cfg.Node.P2P.BootstrapNodes = asDefault
	cfg.Node.P2P.BootstrapNodesV5 = asDefault

	// Load config file (medium priority)
	if configFile != "" {
		if err := LoadAllConfigs(configFile, &cfg); err != nil {
			return &cfg, err
		}
	}

	// Apply flags (high priority)
	var err error
	cfg.Opera = gossipConfigWithFlags(ctx, cfg.Opera)
	err = SetNodeConfig(ctx, &cfg.Node)
	if err != nil {
		return nil, err
	}

	if err := cfg.Opera.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func MakeAllConfigs(ctx *cli.Context) (*Config, error) {
	return MakeAllConfigsFromFile(ctx, ctx.GlobalString(flags.ConfigFileFlag.Name))
}

func DefaultNodeConfig() node.Config {
	cfg := NodeDefaultConfig
	cfg.Name = ClientIdentifier
	cfg.Version = version.StringWithCommit()
	cfg.HTTPModules = append(cfg.HTTPModules, "eth", "dag", "abft", "web3")
	cfg.WSModules = append(cfg.WSModules, "eth", "dag", "abft", "web3")
	cfg.IPCPath = "sonic.ipc"
	return cfg
}
