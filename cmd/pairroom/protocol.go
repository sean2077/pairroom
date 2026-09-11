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

	hostFlag := flags.String("host-mode", "embedded", "Room host mode: embedded or native")
	jsonFlag := flags.Bool("json", false, "emit the contract as JSON")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: pairroom protocol [--actor claude|codex] [--json]")
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

	host := model.HostMode(*hostFlag)
	if !host.Valid() {
		return fmt.Errorf("invalid host-mode %q", host)
	}
	resolve := protocol.Resolve
	if host == model.HostNative {
		resolve = protocol.ResolveNative
	}
	contract, err := resolve(protocol.Selection{Actor: model.ActorID(strings.TrimSpace(*actorFlag))})
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
