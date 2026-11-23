//go:build darwin

// Command carrier carries one sandbox's traffic on this Mac. The app
// runs one for each running sandbox, under the Carrier Wall.
//
// The sandbox's packets come in on standard input and go back on
// standard output, as the Packet stream. The Carrier ends the sandbox's
// connections in a network stack of its own and makes each again on a
// socket bound to the Interface in use: the first of the sandbox's
// Allowed Interfaces that is there, up, and has an address. It dials
// only public addresses, listens on nothing, resolves no name, and
// speaks no protocol of its own to anyone.
//
// The app tells it what to carry on descriptor 3, as JSON lines: first
// the sandbox's configuration, then each of the gate's judgements. The
// Carrier says on descriptor 4 which Interface is in use. When either
// stream from the app ends, it resets every connection and exits.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"zeolite/internal/sandbox"
)

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "carrier: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	control := bufio.NewReaderSize(os.NewFile(3, "control"), 64<<10)
	reports := json.NewEncoder(os.NewFile(4, "reports"))

	line, err := control.ReadBytes('\n')
	if err != nil {
		fail("no configuration: %v", err)
	}
	var cfg sandbox.CarrierConfig
	if err := json.Unmarshal(line, &cfg); err != nil {
		fail("the configuration could not be read: %v", err)
	}
	c, err := newCarrier(cfg)
	if err != nil {
		fail("%v", err)
	}
	var reporting sync.Mutex
	c.report = func(r sandbox.CarrierReport) {
		reporting.Lock()
		defer reporting.Unlock()
		reports.Encode(r)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go c.watch(ctx)
	// The gate's judgements, for as long as the app sends them.
	go func() {
		defer cancel()
		for {
			line, err := control.ReadBytes('\n')
			if err != nil {
				return
			}
			var j sandbox.CarrierJudgement
			if json.Unmarshal(line, &j) == nil {
				c.judge(j, time.Now())
			}
		}
	}()
	if err := c.carry(ctx, os.Stdin, os.Stdout); err != nil {
		fail("the Packet stream broke: %v", err)
	}
	fmt.Fprintf(os.Stderr, "carrier: ended: carried %d flows, did not carry %d, dropped %d packets unread\n",
		c.carriedFlows.Load(), c.refusedFlows.Load(), c.droppedPackets.Load())
}
