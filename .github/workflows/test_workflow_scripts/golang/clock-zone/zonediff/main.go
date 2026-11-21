//go:build ignore

// Command zonediff prints the first instant from 1970 to 2038 at which two
// zone files read different offsets from UTC, with each one's offset there,
// for the clock-zone e2e (../proven-clock-zone-linux.sh): its steps of a
// zone file of another release of IANA's data than keploy's own show
// something only where the two releases read the zone apart, which this
// reads from the files themselves, never from the day the lane runs nor
// from a C library's or a date command's reading of a TZ. It reads them as
// keploy's own zone check does (models.ZoneRulesDiffer: each file at every
// change of offset either makes), so a difference of any length is found.
//
//	zonediff FILE-A FILE-B
//
// It exits 1, printing nothing, where the two read one offset at every
// instant from 1970 to 2038, and 2 where it cannot read either.
package main

import (
	"fmt"
	"os"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: zonediff FILE-A FILE-B")
		os.Exit(2)
	}
	a, b := load(os.Args[1]), load(os.Args[2])
	from := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2038, 1, 1, 0, 0, 0, 0, time.UTC)
	at, differ := models.ZoneRulesDiffer(a, b, from, to)
	if !differ {
		os.Exit(1)
	}
	t := at.UTC()
	fmt.Printf("%s %s %s\n", t.Format(time.RFC3339), t.In(a).Format("-07:00"), t.In(b).Format("-07:00"))
}

func load(p string) *time.Location {
	data, err := os.ReadFile(p)
	if err == nil {
		var loc *time.Location
		if loc, err = time.LoadLocationFromTZData(p, data); err == nil {
			return loc
		}
	}
	fmt.Fprintln(os.Stderr, "zonediff:", err)
	os.Exit(2)
	return nil
}
