package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var version = "dev"

type globalOptions struct {
	configFile string
	authFile   string
	baseURL    string
	apiKey     string
	logLevel   string
	logFormat  string
}

var options globalOptions

func Execute() int {
	root := newRootCommand()
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		return 1
	}
	return 0
}

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "opencode-proxy",
		Short:         "Use OpenCode Zen models from Claude Code and Anthropic-compatible clients",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	flags := root.PersistentFlags()
	flags.StringVar(&options.configFile, "config", "", "configuration file")
	flags.StringVar(&options.authFile, "auth-file", "", "OpenCode credential file")
	flags.StringVar(&options.baseURL, "base-url", "", "OpenCode Zen API base URL")
	flags.StringVar(&options.apiKey, "api-key", "", "OpenCode Zen API key")
	flags.StringVar(&options.logLevel, "log-level", "", "log level")
	flags.StringVar(&options.logFormat, "log-format", "", "text or json logging")

	root.AddCommand(
		newModelsCommand(),
		newServeCommand(),
		newVersionCommand(),
	)
	root.InitDefaultCompletionCmd()
	return root
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run: func(command *cobra.Command, _ []string) {
			_, _ = fmt.Fprintln(command.OutOrStdout(), "opencode-proxy "+version)
		},
	}
}
