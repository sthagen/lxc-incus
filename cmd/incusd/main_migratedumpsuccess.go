package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
)

type cmdMigratedumpsuccess struct {
	global *cmdGlobal
}

func (c *cmdMigratedumpsuccess) command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = "migratedumpsuccess <operation> <secret>"
	cmd.Short = "Tell the daemon that a particular CRIU dump succeeded"
	cmd.Long = `Description:
  Tell the daemon that a particular CRIU dump succeeded

  This internal command is used from the CRIU dump script and is
  called as soon as the script is done running.
`
	cmd.RunE = c.run
	cmd.Hidden = true

	return cmd
}

func (c *cmdMigratedumpsuccess) run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	if len(args) < 2 {
		_ = cmd.Help()

		if len(args) == 0 {
			return nil
		}

		return errors.New("Missing required arguments")
	}

	// Only root should run this
	if os.Geteuid() != 0 {
		return errors.New("This must be run as root")
	}

	clientArgs := incus.ConnectionArgs{
		SkipGetServer: true,
	}

	d, err := incus.ConnectIncusUnix("", &clientArgs)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/websocket?secret=%s", strings.TrimPrefix(args[0], "/1.0"), args[1])
	conn, err := d.RawWebsocket(url)
	if err != nil {
		return err
	}

	_ = conn.Close()

	resp, _, err := d.RawQuery("GET", fmt.Sprintf("%s/wait", args[0]), nil, "")
	if err != nil {
		return err
	}

	op, err := resp.MetadataAsOperation()
	if err != nil {
		return err
	}

	if op.StatusCode == api.Success {
		return nil
	}

	return errors.New(op.Err)
}
