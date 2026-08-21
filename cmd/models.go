package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newModelsCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "models",
		Short: "List models available through OpenCode Zen",
		RunE: func(command *cobra.Command, _ []string) error {
			runtime, err := newRuntime()
			if err != nil {
				return err
			}
			models, err := runtime.client.Models(command.Context())
			if err != nil {
				return err
			}
			for _, model := range models {
				if _, err := fmt.Fprintln(command.OutOrStdout(), model.ID); err != nil {
					return err
				}
			}
			return nil
		},
	}
	return command
}
