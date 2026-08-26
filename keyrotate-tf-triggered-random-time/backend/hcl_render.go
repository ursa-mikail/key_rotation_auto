package main

import (
	"fmt"
	"strconv"
	"strings"
)

// terraformResourceType and terraformResourceName are what the
// commented-out block in terraform/main.tf actually names itself --
// `resource "ursa_keyset" "unit_1_keyset"`. Kept here as constants so
// this file's rendering and that file's comment can't drift apart
// silently.
const (
	terraformResourceType = "ursa_keyset"
	terraformResourceName = terraformKeysetID
)

// renderKeysetResourceHCL produces the actual HCL a real deployment's
// resource would show, populated with LIVE current values -- not the
// static main.tf source (which only ever shows variable declarations
// and a commented-out example), and not the raw tfvars/output JSON
// either. This is specifically what shows up in the frontend's "live
// resource" tab, re-rendered fresh on every status call from whatever
// listKeys currently returns, so it updates every time the underlying
// keyset does -- at most 1 second of staleness, same as everything
// else on the dashboard (see README, "Real-time display").
func renderKeysetResourceHCL(keys []KeyRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "resource %q %q {\n", terraformResourceType, terraformResourceName)
	fmt.Fprintf(&b, "  id   = %s\n", strconv.Quote(terraformKeysetID))
	fmt.Fprintf(&b, "  type = %s\n", strconv.Quote(terraformKeyType))

	if len(keys) == 0 {
		b.WriteString("  keys = [] # no genesis key yet\n")
		b.WriteString("}\n")
		return b.String()
	}

	b.WriteString("  keys = [\n")
	for i, k := range keys {
		b.WriteString("    {\n")
		fmt.Fprintf(&b, "      label      = %s\n", strconv.Quote(k.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z")))
		fmt.Fprintf(&b, "      expiration = %s\n", strconv.Quote(k.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z")))
		fmt.Fprintf(&b, "      length     = %d\n", terraformKeyBits)
		fmt.Fprintf(&b, "      status     = %s\n", strconv.Quote("ENABLED"))
		fmt.Fprintf(&b, "      primary    = %t\n", k.Status == "primary")
		if i == len(keys)-1 {
			b.WriteString("    }\n")
		} else {
			b.WriteString("    },\n")
		}
	}
	b.WriteString("  ]\n")
	b.WriteString("}\n")
	return b.String()
}
