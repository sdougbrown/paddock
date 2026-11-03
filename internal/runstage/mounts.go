package runstage

import (
	"fmt"

	"github.com/sdougbrown/paddock/internal/plan"
)

// CheckMountTargets rejects the plan's mounts that fail the mount rule or
// target a path the engine mounts itself: engineOwned lists the directory
// roots whose subtrees are engine-owned.
func CheckMountTargets(p *plan.Plan, engineOwned []string) error {
	for _, m := range p.Mounts {
		if err := m.Check(); err != nil {
			return fmt.Errorf("plan mount %s: %w", m.Host, err)
		}
		if AtOrUnder(m.Guest, engineOwned...) {
			return fmt.Errorf("plan mount %q targets a paddock-managed guest path", m.Guest)
		}
	}
	return nil
}
