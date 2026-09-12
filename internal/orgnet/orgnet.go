// Package orgnet owns the per-organization overlay network: its name, and the
// startup migration that moves an installation from the single shared network
// onto one network per organization.
package orgnet

import "fmt"

// Name is the overlay network of an organization. One network per organization
// is what keeps one tenant's services from resolving and reaching another's:
// on a shared overlay every service name is reachable by every container.
func Name(orgID int64) string { return fmt.Sprintf("krill-org-%d", orgID) }
