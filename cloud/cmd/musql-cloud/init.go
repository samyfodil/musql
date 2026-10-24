package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/samyfodil/musql/cloud/server"
)

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	path := fs.String("config", "./config.yaml", "path to config.yaml")
	email := fs.String("admin-email", "", "admin email (required)")
	password := fs.String("admin-password", "", "admin password; prompted for when omitted")
	fs.Parse(args)
	if *email == "" {
		return errors.New("init: --admin-email is required")
	}
	b, err := os.ReadFile(*path)
	if err != nil {
		return err
	}
	cfg, err := server.ParseConfig(b)
	if err != nil {
		return err
	}
	pw := *password
	if pw == "" {
		if pw, err = promptPassword(os.Stdin, os.Stderr); err != nil {
			return err
		}
	}
	if err := server.Init(context.Background(), cfg, *email, pw); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "admin %s is ready\n", *email)
	return nil
}

func promptPassword(in *os.File, out io.Writer) (string, error) {
	if !term.IsTerminal(int(in.Fd())) {
		return readPasswordLine(in)
	}
	read := func(prompt string) (string, error) {
		fmt.Fprint(out, prompt)
		b, err := term.ReadPassword(int(in.Fd()))
		fmt.Fprintln(out)
		return string(b), err
	}
	pw, err := read("Admin password: ")
	if err != nil {
		return "", err
	}
	again, err := read("Confirm password: ")
	if err != nil {
		return "", err
	}
	if pw != again {
		return "", errors.New("passwords do not match")
	}
	if pw == "" {
		return "", errors.New("empty password")
	}
	return pw, nil
}

func readPasswordLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		return "", errors.New("no password on stdin")
	}
	pw := strings.TrimRight(line, "\r\n")
	if pw == "" {
		return "", errors.New("empty password")
	}
	return pw, nil
}
