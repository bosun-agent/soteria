// Copyright 2021-2022 - offen.software <hioffen@posteo.de>
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"flag"
)

func main() {
	foreground := flag.Bool("foreground", false, "run the tool in the foreground")
	profile := flag.String("profile", "", "collect runtime metrics and log them periodically on the given cron expression")
	httpAddr := flag.String("http-addr", ":8080", "address to listen on when running the serve command")
	flag.Parse()
	additionalArgs := flag.Args()
	c := newCommand()

	if len(additionalArgs) > 0 {
		switch additionalArgs[0] {
		case "print-config":
			c.must(runPrintConfig())
			return
		case "serve":
			// serve exposes the backup run over HTTP so an approval workflow can
			// trigger it and wait for a verified artifact. It never schedules
			// runs on its own.
			c.must(runServe(*httpAddr))
			return
		default:
			panic("unknown command: " + additionalArgs[0])
		}
	}
	if *foreground {
		opts := foregroundOpts{
			profileCronExpression: *profile,
		}
		c.must(c.runInForeground(opts))
	} else {
		c.must(c.runAsCommand())
	}
}
