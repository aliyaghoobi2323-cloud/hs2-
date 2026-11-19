package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// sec370 (sec353 1c, H2): the connection code carries shared_key. Passed in
// argv it lands in root's shell history and, while join runs, in
// /proc/<pid>/cmdline, which every local user can read. So join also takes
// the code from:
//
//	hs2 tunnel join                     a no-echo prompt (a terminal), else stdin
//	hs2 tunnel join -  < code.txt       stdin
//	hs2 tunnel join --code-file F       a file (- = stdin; /dev/fd/3 from the menu)
//
// The argv form keeps working, with a warning on stderr.

const joinCodeMax = 64 << 10

// readCodeLine reads one line (at most joinCodeMax bytes) byte by byte, so
// nothing after the newline is consumed: the wizard's later prompts read the
// same stdin.
func readCodeLine(r io.Reader) (string, error) {
	var b []byte
	one := make([]byte, 1)
	for len(b) < joinCodeMax {
		n, err := r.Read(one)
		if n == 1 {
			if one[0] == '\n' {
				if strings.TrimSpace(string(b)) == "" {
					b = b[:0] // skip blank lines before the code
					continue
				}
				break
			}
			b = append(b, one[0])
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return "", err
		}
	}
	return cleanCode(string(b)), nil
}

// cleanCode trims spaces and quotes, and a pasted "hs2 tunnel join " prefix.
func cleanCode(s string) string {
	s = strings.TrimSpace(s)
	for _, p := range []string{"hs2 tunnel join ", "tunnel join "} {
		s = strings.TrimSpace(strings.TrimPrefix(s, p))
	}
	s = strings.Trim(s, `'"`)
	return strings.TrimSpace(s)
}

func readCodeFile(path string, stdin io.Reader) (string, error) {
	if path == "-" {
		return readCodeLine(stdin)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return readCodeLine(f)
}

// joinCodeSource resolves the code: argCode is the positional argument ("" =
// none, "-" = stdin), codeFile the --code-file flag. prompt is called (to show
// a no-echo prompt) when the code comes from a terminal. warn gets the argv
// warning.
func joinCodeSource(argCode, codeFile string, stdin io.Reader, stdinTTY bool, prompt func() (string, error), warn io.Writer) (string, error) {
	var code string
	var err error
	switch {
	case codeFile != "":
		if argCode != "" {
			return "", errors.New(tr("give the code either as an argument or with --code-file, not both", "کد را یا به‌صورت آرگومان بدهید یا با --code-file، نه هر دو"))
		}
		code, err = readCodeFile(codeFile, stdin)
	case argCode == "-":
		code, err = readCodeLine(stdin)
	case argCode == "" && stdinTTY:
		code, err = prompt()
	case argCode == "":
		code, err = readCodeLine(stdin)
	default:
		fmt.Fprintln(warn, tr("WARN: the code (it holds the shared key) was given on the command line: it is in your shell history and visible in ps while this runs. Next time run `hs2 tunnel join` and paste it, or `hs2 tunnel join --code-file FILE`.",
			"هشدار: کد (که کلید مشترک را دارد) در خط فرمان داده شد: در تاریخچهٔ shell می‌ماند و تا پایان اجرا در ps دیده می‌شود. دفعهٔ بعد `hs2 tunnel join` را اجرا کنید و کد را بچسبانید، یا `hs2 tunnel join --code-file FILE`."))
		code = argCode
	}
	if err != nil {
		return "", err
	}
	if code == "" {
		return "", errors.New(tr("no code given — usage: hs2 tunnel join [<code> | - | --code-file FILE] [flags]", "کدی داده نشد — استفاده: hs2 tunnel join [<code> | - | --code-file FILE] [flags]"))
	}
	return code, nil
}

// promptCodeNoEcho asks for the code on the terminal with echo off.
func promptCodeNoEcho() (string, error) {
	fmt.Fprint(os.Stderr, tr("Paste the hs2:// code (not shown), then Enter: ", "کد hs2:// را بچسبانید (نمایش داده نمی‌شود) و Enter بزنید: "))
	stty("-echo")
	stop := restoreOnSignal(func() { stty("echo"); fmt.Fprintln(os.Stderr) }, os.Exit)
	defer func() {
		stop()
		stty("echo")
		fmt.Fprintln(os.Stderr)
	}()
	return readCodeLine(os.Stdin)
}

// restoreOnSignal (b376): Ctrl-C (or a kill) at the no-echo prompt must not
// leave the terminal without echo: on SIGINT/SIGTERM/SIGHUP it runs restore,
// then exits 128+signal as the signal would have. stop ends the watch.
func restoreOnSignal(restore func(), exit func(int)) (stop func()) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		select {
		case s := <-sig:
			restore()
			n := 2
			if ss, ok := s.(syscall.Signal); ok {
				n = int(ss)
			}
			exit(128 + n)
		case <-done:
		}
	}()
	return func() {
		signal.Stop(sig)
		close(done)
	}
}
