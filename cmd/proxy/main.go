package main

import (
	"flag"
	"fmt"
	"os"
	"reverse-proxy-lb/internal/config"
	"reverse-proxy-lb/internal/logging"
	"reverse-proxy-lb/internal/server"
)

var (
	configPath = flag.String("config", "configs/config.yaml", "Path to configuration file")
	validate   = flag.Bool("validate", false, "Load and validate the config file, then exit without starting the server")

	// Override flags. These take precedence over the config file (and env vars) but
	// only when explicitly provided on the command line (tracked via flag.Visit).
	flagHost        = flag.String("host", "", "Override server.host")
	flagPort        = flag.Int("port", 0, "Override server.port")
	flagLogLevel    = flag.String("log-level", "", "Override logging.level")
	flagMetricsPort = flag.Int("metrics-port", 0, "Override metrics.port")
)

// applyFlagOverrides applies command-line overrides for flags the user explicitly set.
func applyFlagOverrides(cfg *config.Config) {
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["host"] {
		cfg.Server.Host = *flagHost
	}
	if set["port"] {
		cfg.Server.Port = *flagPort
	}
	if set["log-level"] {
		cfg.Logging.Level = *flagLogLevel
	}
	if set["metrics-port"] {
		cfg.Metrics.Port = *flagMetricsPort
	}
}

func main() {
	flag.Parse()

	if *validate {
		if _, err := config.Load(*configPath); err != nil {
			fmt.Fprintf(os.Stderr, "config invalid: %s: %v\n", *configPath, err)
			os.Exit(1)
		}
		fmt.Printf("config OK: %s\n", *configPath)
		os.Exit(0)
	}

	logging.Info("Starting Reverse Proxy Load Balancer", map[string]interface{}{
		"config": *configPath,
	})

	cfg, err := config.Load(*configPath)
	if err != nil {
		logging.Error("Failed to load config", map[string]interface{}{
			"error": err.Error(),
		})
		fmt.Fprintf(flag.CommandLine.Output(), "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	applyFlagOverrides(cfg)

	srv := server.New(cfg, *configPath)

	if err := srv.Run(); err != nil {
		logging.Error("Server error", map[string]interface{}{
			"error": err.Error(),
		})
	}
}
