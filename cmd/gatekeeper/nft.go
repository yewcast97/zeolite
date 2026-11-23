package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// kernelRuleset is the ruleset in the kernel, and the lease in it,
// driven with the nft command over netlink.
type kernelRuleset struct {
	// hold is whether the ruleset has a Hold: the set "held" is there.
	hold bool
	// standIns is whether the ruleset has the stand-in addresses' rules:
	// the set "standin" is there.
	standIns bool
}

func nft(stdin string, args ...string) ([]byte, error) {
	cmd := exec.Command("nft", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("nft %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// load installs the ruleset in one transaction.
func (kernelRuleset) load(ruleset string) error {
	_, err := nft(ruleset, "-f", "-")
	return err
}

// renew lets the sandbox's egress pass for lease from now, and what it
// has established for hold from now. One that is zero is left as it is.
func (r kernelRuleset) renew(lease, hold time.Duration) error {
	if !r.hold {
		hold = 0
	}
	script := leaseScript(lease.Milliseconds(), hold.Milliseconds())
	if script == "" {
		return nil
	}
	_, err := nft(script, "-f", "-")
	return err
}

// end ends the lease, and the Hold with it.
func (r kernelRuleset) end() error {
	_, err := nft(endScript(r.hold), "-f", "-")
	return err
}

// endFor ends the lease and the Hold, and in the same transaction has
// the stand-in addresses reached, or not, by what passes under the
// lease that follows. A ruleset without their set reaches none.
func (r kernelRuleset) endFor(standIns bool) error {
	script := endScript(r.hold)
	if r.standIns {
		script += standInScript(standIns)
	}
	_, err := nft(script, "-f", "-")
	return err
}

// leaseElements matches what comes and goes in a listing of the ruleset
// while the ruleset stays the same: the elements of the sets "open" and
// "held", which are the lease and the Hold, and of the set "standin",
// which holds the stand-in addresses' range while the Interface in use
// answers names with them.
var leaseElements = regexp.MustCompile(`(set (?:open|held|standin) \{[^}]*?)\n\s*elements = \{[^}]*\}`)

// rulesetIdentity is a listing with everything that changes while the
// gate runs taken out, so that two listings of the same ruleset are
// equal whether the gate is open or locked.
func rulesetIdentity(listing string) string {
	return leaseElements.ReplaceAllString(listing, "$1")
}

// listing is the live ruleset as text: without the counters' values,
// which nft leaves out when asked, and without the lease and the Hold.
func (kernelRuleset) listing() (string, error) {
	out, err := nft("", "-s", "list", "table", "inet", "zeolite")
	return rulesetIdentity(string(out)), err
}

// hash identifies the live ruleset by its listing.
func (r kernelRuleset) hash() (string, error) {
	listing, err := r.listing()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(listing))
	return hex.EncodeToString(sum[:]), nil
}

// counters reads how many packets passed and were rejected.
func (kernelRuleset) counters() (passed, rejected uint64, err error) {
	out, err := nft("", "-j", "list", "counters", "table", "inet", "zeolite")
	if err != nil {
		return 0, 0, err
	}
	return parseCounters(out)
}

func parseCounters(out []byte) (passed, rejected uint64, err error) {
	var listing struct {
		Nftables []struct {
			Counter *struct {
				Name    string `json:"name"`
				Packets uint64 `json:"packets"`
			} `json:"counter"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(out, &listing); err != nil {
		return 0, 0, err
	}
	for _, o := range listing.Nftables {
		if o.Counter == nil {
			continue
		}
		switch o.Counter.Name {
		case "passed":
			passed = o.Counter.Packets
		case "rejected":
			rejected = o.Counter.Packets
		}
	}
	return passed, rejected, nil
}
