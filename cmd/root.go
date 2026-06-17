package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

// Version is set from main.
var Version = "dev"

var (
	cfgFile   string
	verbose   bool
	debug     bool
	logFile   string
	logFormat string

	log = logrus.New()
)

var rootCmd = &cobra.Command{
	Use:           "google2snipe",
	Short:         "Sync ChromeOS devices from the Google Admin SDK into Snipe-IT",
	SilenceUsage:  true,
	SilenceErrors: true,
	Version:       Version,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Help()
	},
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		level := logrus.WarnLevel
		if verbose {
			level = logrus.InfoLevel
		}
		if debug {
			level = logrus.DebugLevel
		}
		SetLogLevel(level)

		switch logFormat {
		case "json":
			SetLogFormat(&logrus.JSONFormatter{})
		default:
			SetLogFormat(&logrus.TextFormatter{FullTimestamp: true})
		}

		var out io.Writer = os.Stderr
		if logFile != "" {
			f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return fmt.Errorf("open log file: %w", err)
			}
			out = io.MultiWriter(os.Stderr, f)
		}
		SetLogOutput(out)
		return nil
	},
}

func init() {
	RegisterLogger(log)
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "settings.yaml", "config file path")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "info-level logging")
	rootCmd.PersistentFlags().BoolVarP(&debug, "debug", "d", false, "debug-level logging")
	rootCmd.PersistentFlags().StringVar(&logFile, "log-file", "", "also append logs to this file")
	rootCmd.PersistentFlags().StringVar(&logFormat, "log-format", "text", "log format: text|json")
}

// Execute runs the root command.
func Execute() {
	rootCmd.Version = Version
	if err := rootCmd.Execute(); err != nil {
		log.WithError(err).Error("command failed")
		os.Exit(1)
	}
}
