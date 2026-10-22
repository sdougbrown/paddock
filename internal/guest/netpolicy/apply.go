package netpolicy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// nftCandidates are the absolute paths searched for the nft binary. The
// search never consults $PATH, so the guest's environment cannot substitute
// another binary.
var nftCandidates = []string{"/usr/sbin/nft", "/sbin/nft", "/usr/bin/nft", "/bin/nft"}

// NFTPath returns the first nft binary found at a fixed system path.
func NFTPath() (string, error) {
	for _, p := range nftCandidates {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("netpolicy: nft not found in %s (install the nftables package in the image)", strings.Join(nftCandidates, ", "))
}

// Apply loads ruleset with `nft -f -`. nft applies the whole input as one
// transaction: on error nothing in it takes effect.
func Apply(ctx context.Context, ruleset string) error {
	if ruleset == "" {
		return errors.New("netpolicy: empty ruleset")
	}
	nft, err := NFTPath()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, nft, "-f", "-")
	cmd.Stdin = strings.NewReader(ruleset)
	cmd.Env = []string{}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("netpolicy: nft -f -: %w: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}
