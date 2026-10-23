package main

import (
	"fmt"
	"os"
	"time"
)

// cmdCheck says how crontinental reads a spec, and when it would fire: the
// question `systemd-analyze calendar` used to answer, on any platform.
func cmdCheck(o *opts) {
	spec := o.when
	found := false
	if c, err := parseCalendar(spec); err == nil {
		found = true
		fmt.Printf("calendar    %s\n", c.normalized())
		t := now()
		label := "next"
		for i := 0; i < o.count; i++ {
			n := c.next(t)
			if n == nil {
				if i == 0 {
					fmt.Printf("next        never\n")
				}
				break
			}
			fmt.Printf("%-11s %s\n", label, humanize(n))
			label, t = "", *n
		}
	}
	if secs, err := parseTimespan(spec); err == nil {
		found = true
		d := time.Duration(secs) * time.Second
		fmt.Printf("interval    %s (%ds)", d, secs)
		if secs < 60 {
			fmt.Printf(" -- too short for `every`: the tick runs once a minute")
		}
		fmt.Println()
	}
	if t, err := parseWhenAt(spec, now(), time.Local); err == nil {
		found = true
		fmt.Printf("time        %s\n", humanize(&t))
	}
	if !found {
		fmt.Fprintf(os.Stderr, "crontinental: %q is not a calendar spec, an interval or a time\n", spec)
		os.Exit(1)
	}
}
