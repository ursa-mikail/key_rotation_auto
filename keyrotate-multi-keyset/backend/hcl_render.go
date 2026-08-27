package main

import (
	"fmt"
	"strconv"
	"strings"
)

// terraformResourceType is what the commented-out block in
// terraform/main.tf actually names itself for each keyset --
// `resource "ursa_keyset" "<keyset_id>"`. Kept here as a constant so
// this file's rendering and that file's comment can't drift apart
// silently.
const terraformResourceType = "ursa_keyset"

// renderKeysetResourceHCL produces the actual HCL every keyset's
// resource would show, populated with LIVE current values -- one
// block per keyset, not the static main.tf source (which only ever
// shows variable declarations and a commented-out example), and not
// raw JSON either. This is specifically what shows up in the
// frontend's "live resource" tab, re-rendered fresh from whatever
// listKeysetsWithKeys currently returns on every status call.
func renderKeysetResourceHCL(keysets []keysetWithKeys) string {
	if len(keysets) == 0 {
		return "# no keysets yet -- click \"Generate Keysets\" to run batch genesis\n"
	}

	var b strings.Builder
	for i, ks := range keysets {
		fmt.Fprintf(&b, "resource %q %q {\n", terraformResourceType, ks.KeysetID)
		fmt.Fprintf(&b, "  id       = %s\n", strconv.Quote(ks.KeysetID))
		fmt.Fprintf(&b, "  type     = %s\n", strconv.Quote(terraformKeyType))
		fmt.Fprintf(&b, "  rotating = %t\n", ks.Rotating)
		fmt.Fprintf(&b, "  revoked  = %t # flagged for emergency renewal at genesis\n", ks.Revoked)

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
