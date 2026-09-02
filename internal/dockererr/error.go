package dockererr

import (
	"context"
	"fmt"
	"strings"
)

// Error is a Docker failure on a remote host, explained: what qifa was doing,
// what Docker said, what that most likely means, what to try, and what the
// host itself reports about its connectivity.
type Error struct {
	Action   string // "pull image", "boot proxy", "issue certificate"…
	Host     string
	Image    string
	Attempts int
	Cause    Cause
	Known    bool
	Report   Report
	Err      error
}

func (e *Error) Unwrap() error { return e.Err }

func (e *Error) Error() string {
	var b strings.Builder

	headline := e.Action
	if e.Image != "" {
		headline += " " + e.Image
	}
	if e.Host != "" {
		headline += " on " + e.Host
	}
	headline += " failed"
	if e.Attempts > 1 {
		headline += fmt.Sprintf(" after %d attempts", e.Attempts)
	}
	if e.Known {
		headline += ": " + e.Cause.Summary
	}
	b.WriteString(headline)

	if out := lastLines(remoteOutput(e.Err), 12); out != "" {
		b.WriteString("\n\n  docker said:\n")
		for _, line := range strings.Split(out, "\n") {
			b.WriteString("    " + line + "\n")
		}
	} else {
		b.WriteString(fmt.Sprintf("\n\n  error: %v\n", e.Err))
	}

	if e.Known && e.Cause.Hint != "" {
		b.WriteString("\n  what to check:\n")
		for _, line := range strings.Split(strings.TrimRight(e.Cause.Hint, "\n"), "\n") {
			b.WriteString("    " + strings.ReplaceAll(line, "<registry>", RegistryHost(e.Image)) + "\n")
		}
	}
	if !e.Known {
		note := "\n  qifa did not recognise this failure; `qifa doctor` checks the host's\n  connectivity to the registry if this looks like a network problem.\n"
		if len(e.Report.Checks) > 0 {
			note = "\n  qifa did not recognise this failure; the host diagnostics below\n  are the next place to look.\n"
		}
		b.WriteString(note)
	}

	if diag := e.Report.String(); diag != "" {
		b.WriteString("\n" + diag)
	}
	return strings.TrimRight(b.String(), "\n")
}

// remoteOutput returns the captured remote stderr/stdout, if the error carried
// any.
func remoteOutput(err error) string {
	if err == nil {
		return ""
	}
	if out := Output(err); strings.TrimSpace(out) != "" {
		return out
	}
	return ""
}

func lastLines(s string, n int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	// Pull output is mostly layer progress; the interesting part is the tail.
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Wrap explains a failed Docker operation. When the failure looks like (or
// might be) a network fault, it asks the host to describe its own connectivity
// so that report lands in the same error the operator is already reading —
// there is no second command to run and no lost context.
//
// runner may be nil to skip the host probe.
func Wrap(ctx context.Context, runner Runner, action, host, image string, attempts int, err error) error {
	return wrap(ctx, runner, action, host, image, attempts, err, true)
}

// WrapKnownNetwork explains a failure the same way, but probes the host only
// when the cause is a *recognised* network fault. Use it for operations whose
// failures are usually the user's own doing — a build that fails on a compile
// error should not spend a round trip asking the host about its DNS.
func WrapKnownNetwork(ctx context.Context, runner Runner, action, host, image string, attempts int, err error) error {
	return wrap(ctx, runner, action, host, image, attempts, err, false)
}

func wrap(ctx context.Context, runner Runner, action, host, image string, attempts int, err error, probeUnknown bool) error {
	if err == nil {
		return nil
	}
	cause, known := ClassifyErr(err)
	wrapped := &Error{
		Action:   action,
		Host:     host,
		Image:    image,
		Attempts: attempts,
		Cause:    cause,
		Known:    known,
		Err:      err,
	}
	probe := (known && cause.Network) || (!known && probeUnknown)
	if runner != nil && host != "" && probe && ctx.Err() == nil {
		wrapped.Report = Diagnose(ctx, runner, host, image)
	}
	return wrapped
}
