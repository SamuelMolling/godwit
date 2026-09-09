package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/SamuelMolling/godwit/internal/comment"
)

func newCommentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "comment",
		Short:  "Read a pull request comment (the GitHub Action calls this)",
		Hidden: true,
	}
	cmd.AddCommand(newCommentParseCmd())

	return cmd
}

func newCommentParseCmd() *cobra.Command {
	var want, path string
	cmd := &cobra.Command{
		Use:   "parse",
		Short: "Print the command a comment body names, as key=value lines",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			found, err := comment.Parse(string(body), want)
			if err != nil {
				return exitError{code: ExitActionRefused, msg: err.Error()}
			}
			if found == nil {
				found = &comment.Command{}
			}
			for _, field := range [][2]string{
				{"command", found.Name},
				{"sha", found.Sha},
				{"ack", strings.Join(found.Ack, ",")},
				{"allow-data-loss", commandFlag(found.AllowDataLoss)},
				{"force", commandFlag(found.Force)},
				{"rollout", found.Rollout},
			} {
				fmt.Fprintf(cmd.OutOrStdout(), "%s=%s\n", field[0], field[1])
			}

			return nil
		},
	}
	cmd.Flags().StringVar(&want, "command", "", "accept only this command (empty accepts any)")
	cmd.Flags().StringVar(&path, "body-file", "", "file holding the comment body")
	_ = cmd.MarkFlagRequired("body-file")

	return cmd
}

func commandFlag(set bool) string {
	if set {
		return "true"
	}

	return ""
}
