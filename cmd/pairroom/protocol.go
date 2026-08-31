package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/protocol"
)

func runProtocol(args []string) error {
	return writeProtocol(args, os.Stdout, os.Stderr)
}

func writeProtocol(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("pairroom protocol", flag.ContinueOnError)
	flags.SetOutput(stderr)
	actorFlag := flags.String("actor", "", "limit actor-specific rules to claude or codex")
	roleFlag := flags.String("role", "", "limit role rules to driver, reviewer, or peer")
	routingFlag := flags.String("routing", "", "turns (legacy values are accepted as aliases)")
	jsonFlag := flags.Bool("json", false, "emit the contract as JSON")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: pairroom protocol [--actor claude|codex] [--role driver|reviewer|peer] [--routing turns] [--json]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}

	contract, err := protocol.Resolve(protocol.Selection{
		Actor:       model.ActorID(strings.TrimSpace(*actorFlag)),
		Role:        model.ParticipantRole(strings.TrimSpace(*roleFlag)),
		RoutingMode: model.RoutingMode(strings.TrimSpace(*routingFlag)),
	})
	if err != nil {
		return err
	}
	if *jsonFlag {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(contract)
	}
	_, err = io.WriteString(stdout, contract.Text())
	return err
}
