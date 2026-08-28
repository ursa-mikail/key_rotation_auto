package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

const terraformResourceType = "ursa_keyset"

// renderKeysetResourceHCL produces the actual HCL every keyset's
// resource would show, populated with LIVE current values -- one
// block per keyset, regenerated fresh on every status call. Includes
// `terminated`/`last_outcome`/`last_trigger` so the "revoked and
// rotated" vs "revoked and terminated" status is visible directly in
// this view too, not just in the tfvars/output json.
func renderKeysetResourceHCL(keysets []keysetWithKeys) string {
	if len(keysets) == 0 {
		return "# no keysets yet -- click \"Generate Keysets\" to run batch genesis\n"
	}

	var b strings.Builder
	for i, ks := range keysets {
		fmt.Fprintf(&b, "resource %q %q {\n", terraformResourceType, ks.KeysetID)
		fmt.Fprintf(&b, "  id         = %s\n", strconv.Quote(ks.KeysetID))
		fmt.Fprintf(&b, "  type       = %s\n", strconv.Quote(terraformKeyType))
		fmt.Fprintf(&b, "  terminated = %t\n", ks.Terminated)
		if ks.LastEventOutcome != "" {
			fmt.Fprintf(&b, "  last_outcome = %s # via %s trigger, %s\n",
				strconv.Quote(ks.LastEventOutcome), ks.LastEventTrigger, formatEventTime(ks.LastEventAt))
		}

		if len(ks.Keys) == 0 {
			b.WriteString("  keys = [] # no key yet\n")
		} else {
			b.WriteString("  keys = [\n")
			for j, k := range ks.Keys {
				b.WriteString("    {\n")
				fmt.Fprintf(&b, "      label      = %s\n", strconv.Quote(k.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z")))
				fmt.Fprintf(&b, "      expiration = %s\n", strconv.Quote(k.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z")))
				fmt.Fprintf(&b, "      length     = %d\n", terraformKeyBits)
				fmt.Fprintf(&b, "      status     = %s\n", strconv.Quote("ENABLED"))
				fmt.Fprintf(&b, "      primary    = %t\n", k.Status == "primary")
				if j == len(ks.Keys)-1 {
					b.WriteString("    }\n")
				} else {
					b.WriteString("    },\n")
				}
			}
			b.WriteString("  ]\n")
		}
		if i == len(keysets)-1 {
			b.WriteString("}\n")
		} else {
			b.WriteString("}\n\n")
		}
	}
	return b.String()
}

func formatEventTime(t *time.Time) string {
	if t == nil {
		return "unknown time"
	}
	return t.UTC().Format("2006-01-02T15:04:05Z")
}
