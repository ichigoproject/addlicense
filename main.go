package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
)

const helpText = `Usage: addlicense [flags] pattern [pattern ...]

The program ensures source code files have copyright license headers
by scanning directory patterns recursively.

It modifies all source files in place and avoids adding a license header
to any file that already has one.

The pattern argument can be provided multiple times, and may also refer
to single files.

Flags:
`

var (
	skipExtensionFlags skipExtensionFlag

	holder    = flag.String("c", "Ichigo Project", "copyright holder")
	license   = flag.String("l", "gnu", "license type: apache, bsd, mit, mpl, gnu")
	licensef  = flag.String("f", "", "license file")
	year      = flag.String("y", fmt.Sprint(time.Now().Year()), "copyright year(s)")
	verbose   = flag.Bool("v", false, "verbose mode: print the name of the files that are modified")
	files     = flag.Int("p", 1000, "number of files to open in parallel")
	checkonly = flag.Bool("check", false, "check only mode: verify presence of license headers and exit with non-zero code if missing")
)

type skipExtensionFlag []string

func (i *skipExtensionFlag) String() string {
	return fmt.Sprint(*i)
}

func (i *skipExtensionFlag) Set(value string) error {
	*i = append(*i, value)
	return nil
}

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, helpText)
		flag.PrintDefaults()
	}
	flag.Var(&skipExtensionFlags, "skip", "To skip files to check/add the header file, for example: -skip rb -skip go")
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(1)
	}

	data := &copyrightData{
		Year:   *year,
		Holder: *holder,
	}

	var t *template.Template
	if *licensef != "" {
		d, err := ioutil.ReadFile(*licensef)
		if err != nil {
			log.Printf("license file: %v", err)
			os.Exit(1)
		}
		t, err = template.New("").Parse(string(d))
		if err != nil {
			log.Printf("license file: %v", err)
			os.Exit(1)
		}
	} else {
		t = licenseTemplate[*license]
		if t == nil {
			log.Printf("unknown license: %s", *license)
			os.Exit(1)
		}
	}

	// process at most 1000 files in parallel (by default)
	ch := make(chan *file, *files)
	done := make(chan struct{})
	go func() {
		var wg errgroup.Group
		for f := range ch {
			f := f // https://golang.org/doc/faq#closures_and_goroutines
			wg.Go(func() error {
				if *checkonly {
					// Check if file extension is known
					lic, err := licenseHeader(f.path, t, data)
					if err != nil {
						log.Printf("%s: %v", f.path, err)
						return err
					}
					if lic == nil { // Unknown fileExtension
						return nil
					}
					// Check if file has a license
					hasLicense, err := fileHasLicense(f.path)
					if err != nil {
						log.Printf("%s: %v", f.path, err)
						return err
					}
					if !hasLicense {
						fmt.Printf("%s\n", f.path)
						return errors.New("missing license header")
					}
				} else {
					modified, err := addLicense(f.path, f.mode, t, data)
					if err != nil {
						log.Printf("%s: %v", f.path, err)
						return err
					}
					if *verbose && modified {
						log.Printf("%s modified", f.path)
					}
				}
				return nil
			})
		}
		err := wg.Wait()
		close(done)
		if err != nil {
			os.Exit(1)
		}
	}()

	for _, d := range flag.Args() {
		walk(ch, d)
	}
	close(ch)
	<-done
}

type file struct {
	path string
	mode os.FileMode
}

func walk(ch chan<- *file, start string) {
	filepath.Walk(start, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			log.Printf("%s error: %v", path, err)
			return nil
		}
		if fi.IsDir() {
			return nil
		}
		for _, skip := range skipExtensionFlags {
			if strings.TrimPrefix(filepath.Ext(fi.Name()), ".") == skip || fi.Name() == skip {
				log.Printf("%s: skipping this file", fi.Name())
				return nil
			}
		}
		ch <- &file{path, fi.Mode()}
		return nil
	})
}

// addLicense add a license to the file if missing.
//
// It returns true if the file was updated.
func addLicense(path string, fmode os.FileMode, tmpl *template.Template, data *copyrightData) (bool, error) {
	var lic []byte
	var err error
	lic, err = licenseHeader(path, tmpl, data)
	if err != nil || lic == nil {
		return false, err
	}

	b, err := ioutil.ReadFile(path)
	if err != nil {
		return false, err
	}
	if hasLicense(b) || isGenerated(b) {
		return false, err
	}

	line := hashBang(b)
	if len(line) > 0 {
		b = b[len(line):]
		if line[len(line)-1] != '\n' {
			line = append(line, '\n')
		}
		lic = append(line, lic...)
	}
	b = append(lic, b...)
	return true, ioutil.WriteFile(path, b, fmode)
}

// fileHasLicense reports whether the file at path contains a license header.
func fileHasLicense(path string) (bool, error) {
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return false, err
	}
	// If generated, we count it as if it has a license.
	return hasLicense(b) || isGenerated(b), nil
}

func licenseHeader(path string, tmpl *template.Template, data *copyrightData) ([]byte, error) {
	var lic []byte
	var err error
	switch fileExtension(path) {
	default:
		return nil, nil
	case ".c", ".h", ".gv":
		lic, err = prefix(tmpl, data, "/*", " * ", " */")
	case ".js", ".mjs", ".cjs", ".jsx", ".tsx", ".css", ".scss", ".sass", ".tf", ".ts":
		lic, err = prefix(tmpl, data, "/**", " * ", " */")
	case ".cc", ".cpp", ".cs", ".go", ".hh", ".hpp", ".java", ".m", ".mm", ".proto", ".rs", ".scala", ".swift", ".dart", ".groovy", ".kt", ".kts", ".v", ".sv":
		lic, err = prefix(tmpl, data, "", "// ", "")
	case ".py", ".sh", ".yaml", ".yml", ".dockerfile", "dockerfile", ".rb", "gemfile", ".tcl", ".bzl":
		lic, err = prefix(tmpl, data, "", "# ", "")
	case ".el", ".lisp":
		lic, err = prefix(tmpl, data, "", ";; ", "")
	case ".erl":
		lic, err = prefix(tmpl, data, "", "% ", "")
	case ".hs", ".sql", ".sdl":
		lic, err = prefix(tmpl, data, "", "-- ", "")
	case ".html", ".xml", ".vue":
		lic, err = prefix(tmpl, data, "<!--", " ", "-->")
	case ".php":
		lic, err = prefix(tmpl, data, "", "// ", "")
	case ".ml", ".mli", ".mll", ".mly":
		lic, err = prefix(tmpl, data, "(**", "   ", "*)")
	}
	return lic, err
}

func fileExtension(name string) string {
	if v := filepath.Ext(name); v != "" {
		return strings.ToLower(v)
	}
	return strings.ToLower(filepath.Base(name))
}

var head = []string{
	"#!",                       // shell script
	"<?xml",                    // XML declaratioon
	"<!doctype",                // HTML doctype
	"# encoding:",              // Ruby encoding
	"# -*- coding:",            // Python encoding
	"# frozen_string_literal:", // Ruby interpreter instruction
	"<?php",                    // PHP opening tag
}

func hashBang(b []byte) []byte {
	var line []byte
	for _, c := range b {
		line = append(line, c)
		if c == '\n' {
			break
		}
	}
	first := strings.ToLower(string(line))
	for _, h := range head {
		if strings.HasPrefix(first, h) {
			return line
		}
	}
	return nil
}

// go generate: ^// Code generated .* DO NOT EDIT\.$
var goGenerated = regexp.MustCompile(`(?m)^.{1,2} Code generated .* DO NOT EDIT\.$`)

// cargo raze: ^DO NOT EDIT! Replaced on runs of cargo-raze$
var cargoRazeGenerated = regexp.MustCompile(`(?m)^DO NOT EDIT! Replaced on runs of cargo-raze$`)

// isGenerated returns true if it contains a string that implies the file was
// generated.
func isGenerated(b []byte) bool {
	return goGenerated.Match(b) || cargoRazeGenerated.Match(b)
}

func hasLicense(b []byte) bool {
	n := 1000
	if len(b) < 1000 {
		n = len(b)
	}
	return bytes.Contains(bytes.ToLower(b[:n]), []byte("copyright")) ||
		bytes.Contains(bytes.ToLower(b[:n]), []byte("mozilla public"))
}
