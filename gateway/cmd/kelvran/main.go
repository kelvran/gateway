// kelvran is the gateway's companion CLI (RFC-3,
// docs/rfcs/2026-10-09-gateway-kelvran-cli-and-single-user-mode.md): the
// by-hand first run — generate a secret, hash it, author config.yaml —
// becomes `kelvran init`. It is a thin dispatcher in the kelvran-bench
// shape; the logic lives in gateway/internal/cli. It ships inside the
// gateway's release archives, packages and image (/kelvran) beside
// kelvran-gateway, and never links the data plane or OpenTelemetry.
package main

import (
	"crypto/rand"
	"fmt"
	"io"
	"os"

	"github.com/kelvran/gateway/gateway/internal/cli"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches one invocation and returns the exit code: 0 success, 1
// `kelvran: <err>` failures, 2 usage errors (usage on stderr).
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "-version", "--version", "version":
		_, _ = fmt.Fprintln(stdout, buildInfoString(version, commit, date))
		return 0
	case "-h", "--help", "help":
		usage(stderr)
		return 0
	case "init":
		return cli.Init(args[1:], cli.IO{Stdout: stdout, Stderr: stderr, Getenv: os.Getenv, Rand: rand.Reader})
	default:
		_, _ = fmt.Fprintf(stderr, "kelvran: unknown command %q\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `usage: kelvran <command> [flags]

  init       write a minimal, priced config.yaml and the first virtual key (kelvran init -h)
  -version   print the build identity

doctor, keys, connect, status and spend follow in later releases (docs/reference/kelvran-cli.md).
`)
}
