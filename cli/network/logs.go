package network

import (
	"os"

	mnet "github.com/bubunyo/monolith/net"

	"github.com/bubunyo/monolith"
	"github.com/spf13/cobra"
)

var (
	logsLines    int
	logsNoFollow bool
)

var logsCmd = &cobra.Command{
	Use:   "logs <name>",
	Short: "Stream the daemon log for a named network",
	Long: `Stream the daemon log for a named network.

By default, the existing log content is printed and the command keeps
following new output as it is written, like ` + "`tail -F`" + `. Use
--no-follow to exit after printing the existing content, and -n/--lines
to limit the size of the initial backlog.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		path, err := mnet.NetworkLogPath(args[0])
		if err != nil {
			return err
		}
		if err := monolith.EnsureLogReadable(path, "network", args[0]); err != nil {
			return err
		}
		return monolith.StreamLogFile(path, os.Stdout, monolith.TailOptions{
			Lines:  logsLines,
			Follow: !logsNoFollow,
		})
	},
}

func init() {
	logsCmd.Flags().IntVarP(&logsLines, "lines", "n", 0, "number of trailing lines to print before following (0 = all)")
	logsCmd.Flags().BoolVar(&logsNoFollow, "no-follow", false, "exit after printing existing content instead of streaming")
}
