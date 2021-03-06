// Copyright 2020 - MinIO, Inc. All rights reserved.
// Use of this source code is governed by the AGPLv3
// license that can be found in the LICENSE file.

package main

import (
	"flag"
	"fmt"
	"io"
	stdlog "log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/minio/kes"
	"github.com/minio/kes/internal/xterm"

	ui "github.com/gizak/termui/v3"
)

const logCmdUsage = `Usage:
    kes log <command>

Commands:
    trace                  Trace server log events.

Options:
    -h, --help             Show list of command-line options.
`

func log(args []string) {
	cli := flag.NewFlagSet(args[0], flag.ExitOnError)
	cli.Usage = func() { fmt.Fprintf(os.Stderr, logCmdUsage) }
	cli.Parse(args[1:])

	if cli.NArg() == 0 {
		cli.Usage()
		os.Exit(2)
	}

	switch args = cli.Args(); args[0] {
	case "trace":
		logTrace(args)
	default:
		stdlog.Fatalf("Error: %q is not a kes log command. See 'kes log --help'", args[0])
	}
}

const traceLogCmdUsage = `Usage:
    kes log trace [options]

Options:
    --type {audit|error}   Specify the log event type.
                           Valid options are:
                             --type=audit (default)
                             --type=error

    --json                 Print log events as JSON.
    -k, --insecure         Skip X.509 certificate validation during TLS handshake.
    -h, --help             Show list of command-line options.

Subscribes to the KES server {audit | error} log. If standard output is
a terminal it displays a table-view terminal UI that shows the stream of
log events. Otherwise, or when --json is specified, the log events are
written to standard output in JSON format.

Examples:
    $ kes log trace
`

func logTrace(args []string) {
	cli := flag.NewFlagSet(args[0], flag.ExitOnError)
	cli.Usage = func() { fmt.Fprintf(os.Stderr, traceLogCmdUsage) }

	var (
		typeFlag           string
		jsonOutput         bool
		insecureSkipVerify bool
	)
	cli.StringVar(&typeFlag, "type", "audit", "Log event type [ audit | error ]")
	cli.BoolVar(&jsonOutput, "json", false, "Print log events as JSON")
	cli.BoolVar(&insecureSkipVerify, "k", false, "Skip X.509 certificate validation during TLS handshake")
	cli.BoolVar(&insecureSkipVerify, "insecure", false, "Skip X.509 certificate validation during TLS handshake")
	cli.Parse(args[1:])

	if cli.NArg() > 0 {
		stdlog.Fatal("Error: too many arguments")
	}

	client := newClient(insecureSkipVerify)
	switch strings.ToLower(typeFlag) {
	case "audit":
		stream, err := client.AuditLog()
		if err != nil {
			stdlog.Fatalf("Error: failed to connect to audit log: %v", err)
		}
		defer stream.Close()

		if !isTerm(os.Stdout) || jsonOutput {
			closeOn(stream, os.Interrupt, os.Kill)
			for stream.Next() {
				fmt.Println(string(stream.Bytes()))
			}
			stdlog.Fatalf("Error: audit log closed with: %v", stream.Err())
			return
		}
		traceAuditLogWithUI(stream)
	case "error":
		stream, err := client.ErrorLog()
		if err != nil {
			stdlog.Fatalf("Error: failed to connect to error log: %v", err)
		}
		defer stream.Close()

		if !isTerm(os.Stdout) || jsonOutput {
			closeOn(stream, os.Interrupt, os.Kill)
			for stream.Next() {
				fmt.Println(string(stream.Bytes()))
			}
			stdlog.Fatalf("Error: error log closed with: %v", stream.Err())
		}
		traceErrorLogWithUI(stream)
	default:
		stdlog.Fatalf("Error: invalid log type --type: %q", typeFlag)
	}
}

// traceAuditLogWithUI iterates over the audit log
// event stream and prints a table-like UI to STDOUT.
//
// Each event is displayed as a new row and the UI is
// automatically adjusted to the terminal window size.
func traceAuditLogWithUI(stream *kes.AuditStream) {
	table := xterm.NewTable("Time", "Identity", "Status", "API Operations", "Response")
	table.Header()[0].Width = 0.12
	table.Header()[1].Width = 0.15
	table.Header()[2].Width = 0.15
	table.Header()[3].Width = 0.45
	table.Header()[4].Width = 0.12

	table.Header()[0].Alignment = xterm.AlignCenter
	table.Header()[1].Alignment = xterm.AlignCenter
	table.Header()[2].Alignment = xterm.AlignCenter
	table.Header()[3].Alignment = xterm.AlignLeft
	table.Header()[4].Alignment = xterm.AlignCenter

	// Initialize the terminal UI and listen on resize
	// events and Ctrl-C / Escape key events.
	if err := ui.Init(); err != nil {
		stdlog.Fatalf("Error: %v", err)
	}
	defer table.Draw() // Draw the table AFTER closing the UI one more time.
	defer ui.Close()   // Closing the UI cleans the screen.

	go func() {
		events := ui.PollEvents()
		for {
			switch event := <-events; {
			case event.Type == ui.ResizeEvent:
				table.Draw()
			case event.ID == "<C-c>" || event.ID == "<Escape>":
				if err := stream.Close(); err != nil {
					fmt.Fprintln(os.Stderr, fmt.Sprintf("Error: audit log stream closed with: %v", err))
				}
				return
			}
		}
	}()

	var (
		green = color.New(color.FgGreen)
		red   = color.New(color.FgRed)
	)
	table.Draw()
	for stream.Next() {
		event := stream.Event()
		hh, mm, ss := event.Time.Clock()

		var (
			identity = xterm.NewCell(event.Request.Identity)
			status   = xterm.NewCell(fmt.Sprintf("%d %s", event.Response.StatusCode, http.StatusText(event.Response.StatusCode)))
			path     = xterm.NewCell(event.Request.Path)
			reqTime  = xterm.NewCell(fmt.Sprintf("%02d:%02d:%02d", hh, mm, ss))
			respTime *xterm.Cell
		)
		if event.Response.StatusCode == http.StatusOK {
			status.Color = green
		} else {
			status.Color = red
		}

		// Truncate duration values such that we show reasonable
		// time values - like 1.05s or 345.76ms.
		switch {
		case event.Response.Time >= time.Second:
			respTime = xterm.NewCell(event.Response.Time.Truncate(10 * time.Millisecond).String())
		case event.Response.Time >= time.Millisecond:
			respTime = xterm.NewCell(event.Response.Time.Truncate(10 * time.Microsecond).String())
		default:
			respTime = xterm.NewCell(event.Response.Time.Truncate(time.Microsecond).String())
		}

		table.AddRow(reqTime, identity, status, path, respTime)
		table.Draw()
	}
	if err := stream.Err(); err != nil {
		stdlog.Fatalf("Error: audit log stream closed with: %v", err)
	}
}

// traceErrorLogWithUI iterates over the error log
// event stream and prints a table-like UI to STDOUT.
//
// Each event is displayed as a new row and the UI is
// automatically adjusted to the terminal window size.
func traceErrorLogWithUI(stream *kes.ErrorStream) {
	table := xterm.NewTable("Time", "Error")
	table.Header()[0].Width = 0.12
	table.Header()[1].Width = 0.87

	table.Header()[0].Alignment = xterm.AlignCenter
	table.Header()[1].Alignment = xterm.AlignLeft

	// Initialize the terminal UI and listen on resize
	// events and Ctrl-C / Escape key events.
	if err := ui.Init(); err != nil {
		stdlog.Fatalf("Error: %v", err)
	}
	defer table.Draw() // Draw the table AFTER closing the UI one more time.
	defer ui.Close()   // Closing the UI cleans the screen.

	go func() {
		events := ui.PollEvents()
		for {
			switch event := <-events; {
			case event.Type == ui.ResizeEvent:
				table.Draw()
			case event.ID == "<C-c>" || event.ID == "<Escape>":
				if err := stream.Close(); err != nil {
					fmt.Fprintln(os.Stderr, fmt.Sprintf("Error: error log stream closed with: %v", err))
				}
				return
			}
		}
	}()

	table.Draw()
	for stream.Next() {
		// An error event message has the following form: YY/MM/DD hh/mm/ss <message>.
		// We split this message into 3 segments:
		//  1. YY/MM/DD
		//  2. hh/mm/ss
		//  3. <message>
		// The 2nd segment is the day-time and 3rd segment is the actual error message.
		// We replace any '\n' with a whitespace to avoid multi-line table rows.
		segments := strings.SplitN(stream.Event().Message, " ", 3)
		var (
			message *xterm.Cell
			reqTime *xterm.Cell
		)
		if len(segments) == 3 {
			message = xterm.NewCell(strings.ReplaceAll(segments[2], "\n", " "))
			reqTime = xterm.NewCell(segments[1])
		} else {
			hh, mm, ss := time.Now().Clock()

			message = xterm.NewCell(strings.ReplaceAll(stream.Event().Message, "\n", " "))
			reqTime = xterm.NewCell(fmt.Sprintf("%02d:%02d:%02d", hh, mm, ss))
		}
		table.AddRow(reqTime, message)
		table.Draw()
	}
	if err := stream.Err(); err != nil {
		stdlog.Fatalf("Error: error log stream closed with: %v", err)
	}
}

// closeOn closes c if one of the given system signals
// occurs. If c.Close() returns an error this error is
// written to STDERR.
func closeOn(c io.Closer, signals ...os.Signal) {
	sigCh := make(chan os.Signal)
	signal.Notify(sigCh, signals...)

	go func() {
		<-sigCh
		if err := c.Close(); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}()
}
