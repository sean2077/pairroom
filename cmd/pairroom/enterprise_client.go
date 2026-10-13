package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseclient"
	"github.com/sean2077/pairroom/internal/openbrowser"
)

// runEnterpriseClient is dispatched only for endpoint commands; it never loads
// operator DSNs, OIDC secrets or vendor-native sessions.
func runEnterpriseClient(args []string) error {
	if len(args) == 0 {
		return errors.New("enterprise client requires login, status, refresh or logout")
	}
	action := args[0]
	if action != "login" && action != "status" && action != "refresh" && action != "logout" {
		return errors.New("unsupported enterprise endpoint command")
	}
	flags := flag.NewFlagSet("pairroom enterprise "+action, flag.ContinueOnError)
	origin := flags.String("service-origin", "", "exact HTTPS enterprise origin")
	deployment := flags.String("deployment", "", "expected deployment ID")
	directory := flags.String("state-dir", "", "owner-private endpoint state directory")
	ca := flags.String("ca-file", "", "optional PEM CA certificates extending system roots")
	var label *string
	var recoverFlag, retryFlag *bool
	if action == "login" {
		label = flags.String("label", "Native endpoint", "device name shown for browser approval")
		recoverFlag = flags.Bool("recover", false, "explicitly reconcile the original saved enrollment")
	}
	if action == "refresh" {
		recoverFlag = flags.Bool("recover", false, "consume the bounded original-predecessor recovery once")
		retryFlag = flags.Bool("retry", false, "explicitly retry only an operation proven not committed")
	}
	if e := flags.Parse(args[1:]); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return nil
		}
		return e
	}
	if flags.NArg() != 0 || *origin == "" || *deployment == "" {
		return errors.New("enterprise endpoint commands require --service-origin and --deployment, without positional arguments")
	}
	if retryFlag != nil && *retryFlag && *recoverFlag {
		return errors.New("choose either --recover or --retry")
	}
	client, e := enterpriseclient.New(enterpriseclient.Config{Origin: *origin, DeploymentID: *deployment, Directory: *directory, CAFile: *ca, OpenBrowser: openbrowser.Open})
	if e != nil {
		return e
	}
	defer client.Close()
	parent, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	budget := 2 * time.Minute
	if action == "login" {
		budget = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()
	var status enterpriseclient.Status
	switch action {
	case "status":
		status, e = client.CheckStatus(ctx)
	case "login":
		status, e = client.Login(ctx, *label, *recoverFlag)
	case "refresh":
		mode := enterpriseclient.RefreshNew
		if *recoverFlag {
			mode = enterpriseclient.RefreshRecover
		} else if *retryFlag {
			mode = enterpriseclient.RefreshRetry
		}
		status, e = client.Refresh(ctx, mode)
	case "logout":
		status, e = client.Logout(ctx)
	}
	if e != nil {
		return e
	}
	if action == "logout" {
		return json.NewEncoder(os.Stdout).Encode(struct {
			Scope   string                  `json:"scope"`
			Message string                  `json:"message"`
			Status  enterpriseclient.Status `json:"status"`
		}{"local", "Local active credentials were cleared. Server registration and Bindings remain; unresolved private operation journals are retained for explicit reconciliation.", status})
	}
	if err := json.NewEncoder(os.Stdout).Encode(status); err != nil {
		return fmt.Errorf("write enterprise status: %w", err)
	}
	return nil
}
